package avs

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTrustNameMatchesOpenSSL(t *testing.T) {
	caPEM, err := os.ReadFile("testdata/known-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	got, err := TrustName(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	const want = "db1b138a.0"
	if got != want {
		t.Errorf("TrustName = %s, want %s: the device reads a CA only under the name "+
			"openssl -subject_hash_old gives it, and a wrong name is a silently untrusted CA", got, want)
	}
}

func TestTrustNameSHA1MatchesOpenSSL(t *testing.T) {
	caPEM, err := os.ReadFile("testdata/known-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	got, err := TrustNameSHA1(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	const want = "71c97096.0"
	if got != want {
		t.Errorf("TrustNameSHA1 = %s, want %s: libcurl finds a CA in cacerts-sha1 only under the"+
			" name openssl -subject_hash gives it", got, want)
	}
}

func TestIdentityIsReusedNotReminted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")
	first, firstCA, err := Identity(path)
	if err != nil {
		t.Fatal(err)
	}
	second, secondCA, err := Identity(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstCA) != string(secondCA) {
		t.Error("the second call minted a new CA: the one installed in the trust store " +
			"would no longer sign the certificate being served")
	}
	if first.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) != 0 {
		t.Error("the second call minted a new leaf")
	}
}

func TestIdentityServesTheEndpointName(t *testing.T) {
	cert, caPEM, err := Identity(filepath.Join(t.TempDir(), "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.Leaf.VerifyHostname("avs-alexa-4-na.amazon.com"); err != nil {
		t.Errorf("the leaf does not name the endpoint: %v", err)
	}
	if len(cert.Certificate) != 2 {
		t.Fatalf("served %d certificates, want the leaf and its CA: a device that is "+
			"given only the leaf cannot build a chain to the CA it trusts", len(cert.Certificate))
	}
	if _, err := tls.X509KeyPair(nil, nil); err == nil {
		t.Skip()
	}
	if len(caPEM) == 0 {
		t.Error("Identity returned no CA to install")
	}
}

func TestIdentityRepairsAnUnusableBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Identity(path); err != nil {
		t.Errorf("a corrupt bundle left the relay with no identity: %v", err)
	}
}

func TestEveryMintedAuthorityLandsOnTheSameName(t *testing.T) {
	want := []string{TrustDir + "/4c55d173.0", TrustDirSHA1 + "/cbac1ab7.0"}
	for range 2 {
		_, caPEM, _, err := mint()
		if err != nil {
			t.Fatal(err)
		}
		got, err := TrustPaths(caPEM)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("a minted authority is trusted as %v, want %v: deploy/uninstall.py "+
				"removes those paths by name and would leave these behind", got, want)
		}
	}
}
