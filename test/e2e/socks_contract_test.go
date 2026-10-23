package e2e_test

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestSOCKSMethodAndCommandRejections(t *testing.T) {
	addr, _ := setupSocksUDP(t, false, availablePort())
	for _, method := range []byte{2, 128} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		c.Write([]byte{5, 1, method})
		var reply [2]byte
		_, err = io.ReadFull(c, reply[:])
		c.Close()
		if err != nil || reply != [2]byte{5, 255} {
			t.Fatal("unsupported method reply", reply, err)
		}
	}
	for _, command := range []byte{2, 127} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		c.Write([]byte{5, 1, 0})
		var greeting [2]byte
		if _, err = io.ReadFull(c, greeting[:]); err != nil {
			c.Close()
			t.Fatal(err)
		}
		c.Write(append([]byte{5, command, 0}, socksWireAddress("127.0.0.1", 1)...))
		var reply [3]byte
		_, err = io.ReadFull(c, reply[:])
		c.Close()
		if err != nil || reply != [3]byte{5, 7, 0} {
			t.Fatal("unsupported command reply", reply, err)
		}
	}
}

func TestSOCKSAssociationSurvivesUnreachableDestination(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			addr, _ := setupSocksUDP(t, reverse, availablePort())
			_, relay, code := socksUDPControl(t, addr, 0)
			if code != 0 {
				t.Fatal("association refused", code)
			}
			c := socksUDPClient(t, "127.0.0.1")
			dead, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			port := dead.LocalAddr().(*net.UDPAddr).Port
			dead.Close()
			packet := append(append([]byte{0, 0, 0}, socksWireAddress("127.0.0.1", port)...), []byte("unreachable")...)
			if _, err = c.WriteToUDP(packet, relay); err != nil {
				t.Fatal(err)
			}
			c.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
			if _, _, err = c.ReadFromUDP(make([]byte, 128)); err == nil {
				t.Fatal("closed destination replied")
			}
			echo := socksUDPEcho(t, "udp4", "127.0.0.1:0")
			socksUDPRoundTrip(t, c, relay, "127.0.0.1", echo.Port, "same association after destination failure")
		})
	}
}
