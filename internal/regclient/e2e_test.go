package regclient

import (
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/imagefile"
	"github.com/alekzonder/tariboy/internal/storesvc"
)

// buildImage writes a minimal Tariboyfile + prompt, builds it into imagesDir,
// and returns the built ref + digest.
func buildImage(t *testing.T, imagesDir string) (image.Ref, string) {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "Tariboyfile.yaml"),
		[]byte("schema_version: 2\nplugins: []\nskills: []\nprompts:\n  - file: ./task.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "task.md"), []byte("do the thing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	imgFile, err := imagefile.ParseV2(src)
	if err != nil {
		t.Fatalf("imagefile.Parse: %v", err)
	}
	ref, err := image.ParseRef("demo:latest")
	if err != nil {
		t.Fatal(err)
	}
	st := &image.Store{Dir: imagesDir}
	man, err := image.BuildV2(imgFile, imagefile.ResolveRoots{}, ref, st, time.Now, nil)
	if err != nil {
		t.Fatalf("image.Build: %v", err)
	}
	return ref, man.Digest
}

// caFileFor writes the httptest server's cert as a PEM the client can trust.
func caFileFor(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	cert := ts.Certificate()
	if cert == nil {
		t.Fatal("no server certificate")
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// Sanity: the PEM parses.
	if _, err := x509.ParseCertificate(cert.Raw); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStoreRoundTripOverTLS(t *testing.T) {
	storeDir := t.TempDir()
	srv, err := storesvc.New(storesvc.Config{AllowInsecure: true, DataDir: storeDir, DBPath: filepath.Join(storeDir, "s.db")})
	if err != nil {
		t.Fatal(err)
	}
	srv.SeedToken("rw", storesvc.ScopeReadWrite)
	ts := httptest.NewTLSServer(srv.Handler()) // real TLS with a self-signed cert
	defer func() { ts.Close(); srv.Close() }()
	caFile := caFileFor(t, ts)

	// Build a real image in the source store.
	srcImages := t.TempDir()
	ref, digest := buildImage(t, srcImages)

	// Push over TLS with a trusted CA + readwrite token.
	cl, err := NewClient(ts.URL, "rw", caFile)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := Push(srcImages, ref, cl); err != nil || res["pushed"] != true {
		t.Fatalf("Push: %+v err=%v", res, err)
	}

	// Pull into a FRESH image store; digest + manifest must match, image inspects.
	dstImages := t.TempDir()
	res, err := Pull(dstImages, ref, cl)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res["digest"] != digest || res["pulled"] != true {
		t.Fatalf("Pull result = %+v (want digest %s)", res, digest)
	}
	// The pulled image is a valid image store entry (manifest parses, schema ok).
	dst := &image.Store{Dir: dstImages}
	man, err := dst.Inspect(ref)
	if err != nil || man.Digest != digest {
		t.Fatalf("pulled Inspect = %+v, err=%v", man, err)
	}
}

func TestPushRejectedWithoutToken(t *testing.T) {
	storeDir := t.TempDir()
	srv, _ := storesvc.New(storesvc.Config{AllowInsecure: true, DataDir: storeDir, DBPath: filepath.Join(storeDir, "s.db")})
	srv.SeedToken("rw", storesvc.ScopeReadWrite)
	ts := httptest.NewTLSServer(srv.Handler())
	defer func() { ts.Close(); srv.Close() }()
	caFile := caFileFor(t, ts)

	srcImages := t.TempDir()
	ref, _ := buildImage(t, srcImages)

	// No token -> push must fail (401 surfaced as an error).
	cl, _ := NewClient(ts.URL, "", caFile)
	if _, err := Push(srcImages, ref, cl); err == nil {
		t.Fatal("push without a token must be rejected")
	}
}

func TestPullDetectsTampering(t *testing.T) {
	storeDir := t.TempDir()
	srv, _ := storesvc.New(storesvc.Config{AllowInsecure: true, DataDir: storeDir, DBPath: filepath.Join(storeDir, "s.db")})
	srv.SeedToken("rw", storesvc.ScopeReadWrite)
	ts := httptest.NewTLSServer(srv.Handler())
	defer func() { ts.Close(); srv.Close() }()
	caFile := caFileFor(t, ts)

	srcImages := t.TempDir()
	ref, _ := buildImage(t, srcImages)
	cl, _ := NewClient(ts.URL, "rw", caFile)
	if _, err := Push(srcImages, ref, cl); err != nil {
		t.Fatal(err)
	}

	// Tamper the stored blob on the server's disk WITHOUT updating the sidecar,
	// so the GET streams bytes that no longer hash to the advertised digest.
	blobPath, err := (&image.Store{Dir: storeDir}).ArchivePath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, []byte("tampered-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	dstImages := t.TempDir()
	if _, err := Pull(dstImages, ref, cl); err == nil {
		t.Fatal("pull of a tampered blob must fail the digest re-verify")
	}
	// Nothing was installed into the fresh store.
	if (&image.Store{Dir: dstImages}).Exists(ref) {
		t.Fatal("a tampered pull must not install the archive")
	}
}
