package chserver

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	chshare "github.com/jpillora/chisel/share"
	"github.com/jpillora/chisel/share/ccrypto"
	"github.com/jpillora/chisel/share/cio"
	"github.com/jpillora/chisel/share/cnet"
	"github.com/jpillora/chisel/share/settings"
	"github.com/jpillora/requestlog"
	"golang.org/x/crypto/ssh"
	"tack.local/management/protocol/shaping"
)

// Config is the configuration for the chisel service
type Config struct {
	Logger      *cio.Logger   `opts:"-"`
	RequireAuth bool          `opts:"-"`
	KeySeed     string        `opts:"name=key,short=-" help:"(deprecated use --keygen and --keyfile instead) An optional string to seed the generation of a ECDSA public and private key pair. All communications will be secured using this key pair. Share the subsequent fingerprint with clients to enable detection of man-in-the-middle attacks (defaults to the CHISEL_KEY environment variable, otherwise a new key is generate each run)."`
	KeyFile     string        `opts:"name=keyfile,short=-" help:"An optional path to a PEM-encoded SSH private key. When this flag is set, the --key option is ignored, and the provided private key is used to secure all communications. (defaults to the CHISEL_KEY_FILE environment variable). Since ECDSA keys are short, you may also set keyfile to the inline key string itself, exactly as printed by --keygen (a base64 string with a \"ck-\" prefix); no extra base64 encoding is needed."`
	AuthFile    string        `opts:"name=authfile,short=-" help:"An optional path to a users.json file. This file should be an object with users defined like:\n  {\n    \"<user:pass>\": [\"<addr-regex>\",\"<addr-regex>\"]\n  }\nwhen <user> connects, their <pass> will be verified and then each of the remote addresses will be compared against the list of address regular expressions for a match. Patterns are NOT anchored by default: \"10.0.0.1:80\" also matches \"210.0.0.1:8080\", and \".\" matches any character. Anchor your patterns, e.g. \"^10\\.0\\.0\\.1:80$\". The empty string \"\" matches every address. Addresses will always come in the form \"<remote-host>:<remote-port>\" for normal remotes, \"R:<local-interface>:<local-port>\" for reverse port forwarding remotes, and \"socks\" for SOCKS5 proxy access. Note that SOCKS5 access previously bypassed this list; existing authfiles which should allow SOCKS5 must add an entry matching \"socks\" (the empty wildcard \"\" matches everything, including \"socks\"). This file will be automatically reloaded on change. Reloads apply to new connections and to new tunnels of connected clients; established tunnels are not interrupted."`
	Auth        string        `opts:"name=auth,short=-" help:"An optional string representing a single user with full access, in the form of <user:pass>. It is equivalent to creating an authfile with {\"<user:pass>\": [\"\"]}. If unset, it will use the environment variable AUTH."`
	Proxy       string        `opts:"name=backend,short=-" help:"Specifies another HTTP server to proxy requests to when chisel receives a normal HTTP request. Useful for hiding chisel in plain sight. --proxy is accepted as an alias for this flag."`
	Socks5      bool          `opts:"name=socks5,short=-" help:"Allow clients to access the internal SOCKS5 proxy. See chisel client --help for more information."`
	Reverse     bool          `opts:"name=reverse,short=-" help:"Allow clients to specify reverse port forwarding remotes in addition to normal remotes."`
	KeepAlive   time.Duration `opts:"name=keepalive,short=-" help:"An optional keepalive interval. Since the underlying transport is HTTP, in many instances we'll be traversing through proxies, often these proxies will close idle connections. You must specify a time with a unit, for example '5s' or '2m'. Defaults to '25s' (set to 0s to disable)."`
	TLS         TLSConfig     `opts:"mode=embedded"`
}

// Server represent a chisel service
type Server struct {
	*cio.Logger
	config       *Config
	fingerprint  string
	httpServer   *cnet.HTTPServer
	reverseProxy *httputil.ReverseProxy
	sessCount    int32
	sshConfig    *ssh.ServerConfig
	users        *settings.UserIndex
	lifeMu       sync.Mutex
	closing      bool
	started      bool
	cancel       context.CancelFunc
	handlers     sync.WaitGroup
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  settings.EnvInt("WS_BUFF_SIZE", 0),
	WriteBufferSize: settings.EnvInt("WS_BUFF_SIZE", 0),
}

// NewServer creates and returns a new chisel server
func NewServer(c *Config) (*Server, error) {
	server := &Server{
		config:     c,
		httpServer: cnet.NewHTTPServer(),
		Logger:     cio.NewLogger("server"),
	}
	if c.Logger != nil {
		server.Logger = c.Logger
	} else {
		server.Info = true
	}
	server.users = settings.NewUserIndex(server.Logger)
	complete := false
	defer func() {
		if !complete {
			server.users.Close()
		}
	}()
	//pin the --auth user first so authfile reloads cannot drop it
	if c.Auth != "" {
		u := &settings.User{Addrs: []*regexp.Regexp{settings.UserAllowAll}}
		u.Name, u.Pass = settings.ParseAuth(c.Auth)
		if u.Name == "" {
			return nil, server.Errorf("invalid auth string, expected <user>:<pass>")
		}
		server.users.PinUser(u)
	}
	if c.AuthFile != "" {
		if err := server.users.LoadUsers(c.AuthFile); err != nil {
			return nil, err
		}
	}

	var pemBytes []byte
	var err error
	if c.KeyFile != "" {
		var key []byte

		if ccrypto.IsChiselKey([]byte(c.KeyFile)) {
			key = []byte(c.KeyFile)
		} else {
			key, err = os.ReadFile(c.KeyFile)
			if err != nil {
				return nil, server.Errorf("Failed to read key file %s: %s", c.KeyFile, err)
			}
		}

		pemBytes = key
		if ccrypto.IsChiselKey(key) {
			pemBytes, err = ccrypto.ChiselKey2PEM(key)
			if err != nil {
				return nil, server.Errorf("Invalid chisel key: %s", err)
			}
		}
	} else {
		//generate private key (optionally using seed)
		pemBytes, err = ccrypto.Seed2PEM(c.KeySeed)
		if err != nil {
			return nil, server.Errorf("Failed to generate key: %s", err)
		}
	}

	//convert into ssh.PrivateKey
	private, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, server.Errorf("Failed to parse key: %s", err)
	}
	//fingerprint this key
	server.fingerprint = ccrypto.FingerprintKey(private.PublicKey())
	//create ssh config
	server.sshConfig = &ssh.ServerConfig{
		ServerVersion:    "SSH-" + chshare.ProtocolVersion + "-server",
		PasswordCallback: server.authUser,
	}
	server.sshConfig.AddHostKey(private)
	//setup reverse proxy
	if c.Proxy != "" {
		u, err := url.Parse(c.Proxy)
		if err != nil {
			return nil, err
		}
		if u.Host == "" {
			return nil, server.Errorf("Missing protocol (%s)", u)
		}
		server.reverseProxy = httputil.NewSingleHostReverseProxy(u)
		//always use proxy host
		server.reverseProxy.Director = func(r *http.Request) {
			//enforce origin, keep path
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.Host = u.Host
		}
	}
	//print when reverse tunnelling is enabled
	if c.Reverse {
		server.Infof("Reverse tunnelling enabled")
	}
	complete = true
	return server, nil
}

// Run is responsible for starting the chisel service.
// Internally this calls Start then Wait.
func (s *Server) Run(host, port string) error {
	if err := s.Start(host, port); err != nil {
		return err
	}
	return s.Wait()
}

// Start is responsible for kicking off the http server
func (s *Server) Start(host, port string) error {
	return s.StartContext(context.Background(), host, port)
}

// StartContext is responsible for kicking off the http server,
// and can be closed by cancelling the provided context
func (s *Server) StartContext(ctx context.Context, host, port string) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	s.lifeMu.Lock()
	if s.started || s.closing {
		s.lifeMu.Unlock()
		return fmt.Errorf("server already started or closed")
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.started = true
	s.httpServer.BaseContext = func(net.Listener) context.Context { return ctx }
	s.lifeMu.Unlock()
	s.Infof("Fingerprint %s", s.fingerprint)
	if s.users.Len() > 0 {
		s.Infof("User authentication enabled")
	}
	if s.reverseProxy != nil {
		s.Infof("Reverse proxy enabled")
	}
	l, err := s.listener(host, port)
	if err != nil {
		s.Close()
		return err
	}
	h := http.Handler(http.HandlerFunc(s.handleClientHandler))
	if s.Debug {
		o := requestlog.DefaultOptions
		o.TrustProxy = true
		h = requestlog.WrapWith(h, o)
	}
	if shaping.Enabled(s.config.TLS.TrafficShaping) {
		h = shaping.ConfigureServer(s.httpServer.Server, h)
	}
	return s.httpServer.GoServe(ctx, l, h)
}

// Wait waits for the http server to close
func (s *Server) Wait() error {
	err := s.httpServer.Wait()
	s.Close()
	s.handlers.Wait()
	return err
}

// Close forcibly closes the http server
func (s *Server) Close() error {
	s.lifeMu.Lock()
	s.closing = true
	if s.cancel != nil {
		s.cancel()
	}
	started := s.started
	s.lifeMu.Unlock()
	s.users.Close()
	if !started {
		return nil
	}
	return s.httpServer.Close()
}

// GetFingerprint is used to access the server fingerprint
func (s *Server) GetFingerprint() string {
	return s.fingerprint
}

// authUser is responsible for validating the ssh user / password combination
func (s *Server) authUser(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	// check if user authentication is enabled and if not, allow all
	if s.users.Len() == 0 && !s.config.RequireAuth && s.config.AuthFile == "" && s.config.Auth == "" {
		return nil, nil
	}
	// check the user exists and has matching password
	n := c.User()
	user, found := s.users.Get(n)
	if !found || subtle.ConstantTimeCompare([]byte(user.Pass), password) != 1 {
		//info level: operators should see failed attempts (#521)
		s.Infof("Login failed for user %q (%s)", n, c.RemoteAddr())
		return nil, fmt.Errorf("invalid authentication for username: %s", n)
	}
	// pass the username through to the handshake handler, which
	// re-resolves the user (no session state to leak or race)
	return &ssh.Permissions{
		Extensions: map[string]string{"user": n},
	}, nil
}

// AddUser adds a new user into the server user index
func (s *Server) AddUser(user, pass string, addrs ...string) error {
	authorizedAddrs := []*regexp.Regexp{}
	for _, addr := range addrs {
		authorizedAddr, err := regexp.Compile(addr)
		if err != nil {
			return err
		}
		authorizedAddrs = append(authorizedAddrs, authorizedAddr)
	}
	s.users.AddUser(&settings.User{
		Name:  user,
		Pass:  pass,
		Addrs: authorizedAddrs,
	})
	return nil
}

// DeleteUser removes a user from the server user index
func (s *Server) DeleteUser(user string) {
	s.users.Del(user)
}

// ResetUsers in the server user index.
// Use nil to remove all.
func (s *Server) ResetUsers(users []*settings.User) {
	s.users.Reset(users)
}
