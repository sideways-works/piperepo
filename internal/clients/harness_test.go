//go:build containers

// Package clients runs each format's own client against piperepo: real
// packages built by the format's tools, uploaded to two instances on one
// S3 bucket, installed with the signature checked. Run with -tags containers;
// it needs rootless podman.
package clients

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/sideways-works/piperepo/internal/sign"
)

// The images the tests pin, each at its latest release.
const (
	imageSeaweedFS = "docker.io/chrislusf/seaweedfs@sha256:4b42c212543a0acd4c0e675215ecdcaf62a186ef98def76c7e12701871222d15"
	imageDebian    = "docker.io/library/debian@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2"
	imageArch      = "docker.io/library/archlinux@sha256:fa2d4ce3310ddd90a4208e5168be8ad367da5129af1245f38fd2d803bcbe3239"
	imageAlpine    = "docker.io/library/alpine@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf"
	imageFedora    = "docker.io/library/fedora@sha256:a7d72ba2745b100a3351087472e5ee7b2450af7cf331e7673007e3817612839c"
)

const token = "a-test-stores-credential"

// freePort is a port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()

	for {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		port := l.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert,errcheck // a TCP listener
		_ = l.Close()

		// Another project's ports.
		if port < 7100 || port > 7199 {
			return port
		}
	}
}

func podman(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("no podman here")
	}
}

// container runs a container to its end, its output in the test's log.
func container(t *testing.T, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput() //nolint:gosec // the test's own containers
	if err != nil {
		t.Fatalf("podman %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return string(out)
}

// inside runs a shell script in image on the host's network, dir at /w.
func inside(t *testing.T, image, dir, script string) string {
	t.Helper()

	return container(t, "run", "--rm", "--network", "host", "-v", dir+":/w:Z", "-w", "/w", image, "sh", "-ec", script)
}

// bucket starts S3 in a container and makes a bucket: its endpoint.
func bucket(t *testing.T, name string) string {
	t.Helper()

	ports := make([]string, 9)
	for i := range ports {
		ports[i] = strconv.Itoa(freePort(t))
	}

	dir := t.TempDir()
	c := "piperepo-test-s3-" + ports[6]

	t.Cleanup(func() { _ = exec.Command("podman", "rm", "-f", "-t", "0", c).Run() }) //nolint:errcheck,gosec,noctx // the test's own container

	container(t, "run", "-d", "--name", c, "--network", "host", "-v", dir+":/data:Z", "--entrypoint", "/usr/bin/weed", imageSeaweedFS,
		"-logtostderr=true", "server", "-ip=127.0.0.1", "-ip.bind=127.0.0.1", "-dir=/data",
		"-master.port="+ports[0], "-master.port.grpc="+ports[1], "-volume.port="+ports[2], "-volume.port.grpc="+ports[3],
		"-filer", "-filer.port="+ports[4], "-filer.port.grpc="+ports[5],
		"-s3", "-s3.port="+ports[6], "-s3.port.grpc="+ports[7], "-s3.port.iceberg="+ports[8], "-s3.port.lance=0",
		"-master.defaultReplication=000", "-master.volumeSizeLimitMB=64", "-volume.max=100")

	endpoint := "http://127.0.0.1:" + ports[6]
	client := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("k", "s", "")})

	var err error

	for range 120 {
		if _, err = client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(name)}); err == nil {
			return endpoint
		}

		time.Sleep(time.Second)
	}

	t.Fatalf("the bucket: %v", err)

	return ""
}

// binary builds piperepo, static, once per test binary.
func binary(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "piperepo")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, "../../cmd/piperepo") //nolint:gosec // the module's own command
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building piperepo: %v\n%s", err, b)
	}

	return out
}

// store is a running piperepo setup: its instances' URLs.
type store struct {
	format   string
	urls     []string
	dir      string
	bin      string
	endpoint string
}

// serve runs n instances of format on one fresh bucket.
func serve(t *testing.T, format string, n int) *store {
	t.Helper()

	endpoint := bucket(t, "piperepo-"+format)
	dir := t.TempDir()

	for name, body := range map[string]string{"token": token + "\n", "seed": "a test store's signing seed, long enough\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s := &store{format: format, dir: dir, bin: binary(t), endpoint: endpoint}

	for range n {
		s.start(t, nil)
	}

	return s
}

// start runs one more instance with env added: its URL.
func (s *store) start(t *testing.T, env []string) (string, *exec.Cmd) {
	t.Helper()

	port := freePort(t)
	conf := filepath.Join(s.dir, "conf-"+strconv.Itoa(port)+".yaml")
	body := fmt.Sprintf("listen: 127.0.0.1:%d\ns3: {endpoint: %q, bucket: %q, access_key: k, secret_key: s, path_style: true}\ntoken_file: %s\nseed_file: %s\nreconcile: 2s\n",
		port, s.endpoint, "piperepo-"+s.format, filepath.Join(s.dir, "token"), filepath.Join(s.dir, "seed"))

	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	logf, err := os.Create(filepath.Join(s.dir, "log-"+strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(s.bin, "serve", "--format", s.format, "--config", conf) //nolint:gosec,noctx // the test's own build; it outlives no test
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = append(os.Environ(), env...)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill() //nolint:errcheck // it may have ended
		_, _ = cmd.Process.Wait()
		_ = logf.Close()

		if t.Failed() {
			b, _ := os.ReadFile(logf.Name())
			t.Logf("instance %d's log:\n%s", port, b)
		}
	})

	url := "http://127.0.0.1:" + strconv.Itoa(port)

	for start := time.Now(); ; time.Sleep(200 * time.Millisecond) {
		if resp, err := http.Get(url + "/health"); err == nil { //nolint:noctx // a short wait
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				break
			}
		}

		if time.Since(start) > time.Minute {
			t.Fatalf("instance on %d didn't start", port)
		}
	}

	s.urls = append(s.urls, url)

	return url, cmd
}

// request sends one authenticated request to an instance.
func request(t *testing.T, method, url string, body io.Reader) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}

	req.SetBasicAuth("piperun", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(b)
}

// upload sends a file in dir to an instance and wants it taken.
func upload(t *testing.T, url, dir, file string) {
	t.Helper()

	f, err := os.Open(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if code, body := request(t, http.MethodPut, url+"/upload/"+file, f); code != http.StatusCreated {
		t.Fatalf("uploading %s: %d %s", file, code, body)
	}
}

// fetch writes a served path of an instance into dir.
func fetch(t *testing.T, url, path, dir, name string) {
	t.Helper()

	code, body := request(t, http.MethodGet, url+"/"+path, nil)
	if code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, code, body)
	}

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil { //nolint:gosec // read by the client container
		t.Fatal(err)
	}
}

// workdir is a directory a container may write to.
func workdir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // a container's user writes there
		t.Fatal(err)
	}

	return dir
}

// otherKey writes another store's public key into dir as other.asc.
func otherKey(t *testing.T, dir string) {
	t.Helper()

	k, err := sign.OpenPGPFromSeed([]byte("another store's seed, not this one's"), "other")
	if err != nil {
		t.Fatal(err)
	}

	pub, err := k.PublicArmored()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "other.asc"), pub, 0o644); err != nil { //nolint:gosec // read by the client container
		t.Fatal(err)
	}
}

// hostport is an instance's address without its scheme.
func hostport(url string) string { return strings.TrimPrefix(url, "http://") }
