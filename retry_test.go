package main

import (
	"crypto/tls"
	"errors"
	"testing"
)

func TestIsTLSHandshakeError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New(`Post "https://x": tls: failed to verify certificate: x509 blah`), true},
		{errors.New(`tls: handshake failure`), true},
		{errors.New(`remote error: tls: unexpected message`), true},
		{errors.New(`read: connection reset by peer`), true},
		{&tls.CertificateVerificationError{Err: errors.New("x509: expired")}, false},
		{errors.New("tls: failed to verify certificate: x509: certificate is not valid for any names"), true},
		{errors.New("x509: certificate has expired or is not yet valid"), false},
		{errors.New("x509: certificate signed by unknown authority"), false},
		{errors.New("x509: certificate is valid for foo.com, not api.commandcode.ai"), false},
		{errors.New("certificate revoked"), false},
		{errors.New("Post https: tls: handshake failure"), true},
		{errors.New("context deadline exceeded"), false},
		{errors.New("HTTP 403 upgrade_required"), false},
	}
	for _, c := range cases {
		if got := isTLSHandshakeError(c.err); got != c.want {
			t.Errorf("err %v: got %v want %v", c.err, got, c.want)
		}
	}
}
