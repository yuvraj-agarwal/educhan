// Package mediatest starts an in-process Google Cloud Storage emulator for
// tests. It lives in its own package so that both the media package's
// conformance suite and the application's end-to-end tests drive the GCS
// backend the same way, without either importing the other's test files.
//
// Nothing outside a test should import this.
package mediatest

import (
	"net"
	"strconv"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
)

// Bucket is the bucket StartGCS creates.
const Bucket = "educhan-test"

// StartGCS runs a Cloud Storage emulator for the lifetime of t, creates Bucket
// in it, and points the Cloud Storage client at it through
// STORAGE_EMULATOR_HOST. It returns the bucket name.
//
// The emulator is a library rather than a container or a downloaded binary, so
// it is available wherever the tests compile: no cloud credentials, no network,
// and nothing to skip for. Setting STORAGE_EMULATOR_HOST is the emulator's
// documented entry point and switches the client to anonymous access, so tests
// exercise the same media.NewGCSStore path production takes, minus the
// credentials. A failure to start is fatal rather than skipped — a storage
// backend that quietly went untested would be worse than a red build.
//
// STORAGE_EMULATOR_HOST is set with t.Setenv, so a test that calls this must
// not also call t.Parallel.
func StartGCS(t *testing.T) string {
	t.Helper()

	// PublicHost has to be the address the emulator is actually reached on, and
	// it is fixed when the server is built — hence choosing the port first. The
	// client downloads object contents through the XML API, at
	// {host}/{bucket}/{object}, and the emulator only routes that path when the
	// request's Host matches its PublicHost. Left at the default, writes
	// succeed and every read answers "object not found".
	port := freePort(t)
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	server, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme:     "http",
		Host:       "127.0.0.1",
		Port:       uint16(port),
		PublicHost: host,
	})
	if err != nil {
		t.Fatalf("start the Cloud Storage emulator: %v", err)
	}
	t.Cleanup(server.Stop)
	server.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: Bucket})

	t.Setenv("STORAGE_EMULATOR_HOST", "http://"+host)
	return Bucket
}

// freePort reserves a port by binding it and letting go, so the emulator can be
// told its own address before it starts.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address %v", l.Addr())
	}
	return addr.Port
}
