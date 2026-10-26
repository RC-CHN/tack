package browser

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"reflect"
	"sort"

	utls "github.com/refraction-networking/utls"
)

// personalized-v1 is a constrained custom template, not a browser impersonation
// claim. Its recipe is versioned and pinned to uTLS 1.8.2. Do not change this
// recipe behind the same profile name: persisted seeds must remain stable.
func personalizedSpec(seed [32]byte) (utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_133)
	if err != nil {
		return spec, err
	}
	spec.TLSVersMin, spec.TLSVersMax = utls.VersionTLS12, utls.VersionTLS13
	// Retain modern AEAD suites, TLS 1.3, and RSA/ECDSA TLS 1.2 support. Vary
	// only supported capabilities; never invent an extension or reuse key data.
	var modern, older []uint16
	for _, id := range spec.CipherSuites {
		switch id {
		case utls.TLS_AES_128_GCM_SHA256, utls.TLS_AES_256_GCM_SHA384, utls.TLS_CHACHA20_POLY1305_SHA256:
			modern = append(modern, id)
		case utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305, utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305:
			older = append(older, id)
		case utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384, utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384:
			if seed[0]&1 != 0 {
				older = append(older, id)
			}
		}
	}
	order := func(ids []uint16) {
		sort.Slice(ids, func(i, j int) bool {
			a, b := templateRank(seed, "cipher", ids[i]), templateRank(seed, "cipher", ids[j])
			return bytes.Compare(a[:], b[:]) < 0
		})
	}
	order(modern)
	order(older)
	spec.CipherSuites = append(append([]uint16{utls.GREASE_PLACEHOLDER}, modern...), older...)
	type rankedExtension struct {
		extension utls.TLSExtension
		rank      [32]byte
	}
	var extensions []rankedExtension
	var grease []utls.TLSExtension
	for _, e := range spec.Extensions {
		switch e.(type) {
		case *utls.UtlsGREASEExtension:
			grease = append(grease, e)
			continue
		case *utls.StatusRequestExtension:
			if seed[0]&2 == 0 {
				continue
			}
		case *utls.SCTExtension:
			if seed[0]&4 == 0 {
				continue
			}
		case *utls.UtlsCompressCertExtension:
			if seed[0]&8 == 0 {
				continue
			}
		case *utls.ApplicationSettingsExtensionNew:
			if seed[0]&16 == 0 {
				continue
			}
		}
		extensions = append(extensions, rankedExtension{e, templateRank(seed, reflect.TypeOf(e).String(), 0)})
	}
	// Chrome's preset shuffles extensions each time it is created. Ranking by
	// extension type discards that shuffle, without seeding TLS's entropy source.
	sort.Slice(extensions, func(i, j int) bool { return bytes.Compare(extensions[i].rank[:], extensions[j].rank[:]) < 0 })
	spec.Extensions = []utls.TLSExtension{grease[0]}
	for _, e := range extensions {
		spec.Extensions = append(spec.Extensions, e.extension)
	}
	spec.Extensions = append(spec.Extensions, grease[1:]...)
	return spec, nil
}

func templateRank(seed [32]byte, label string, id uint16) [32]byte {
	h := sha256.New()
	h.Write([]byte("tack-personalized-v1\x00"))
	h.Write(seed[:])
	h.Write([]byte(label))
	var value [2]byte
	binary.BigEndian.PutUint16(value[:], id)
	h.Write(value[:])
	var rank [32]byte
	h.Sum(rank[:0])
	return rank
}
