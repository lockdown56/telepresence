package cli_test

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/datawire/ambassador/pkg/dtest"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/datawire/telepresence2/pkg/client"
	"github.com/datawire/telepresence2/pkg/client/cli"
	"github.com/datawire/telepresence2/pkg/version"
)

var testVersion = "v0.1.2-test"
var namespace = fmt.Sprintf("telepresence-%d", os.Getpid())
var proxyOnMatch = regexp.MustCompile(`Proxy:\s+ON`)

var _ = Describe("Telepresence", func() {
	Context("With no daemon running", func() {
		It("Returns version", func() {
			stdout, stderr := telepresence("--version")
			Expect(stderr).To(BeEmpty())
			Expect(stdout).To(Equal(fmt.Sprintf("Client %s", client.DisplayVersion())))
		})
		It("Returns valid status", func() {
			out, _ := telepresence("--status")
			Expect(out).To(ContainSubstring("The telepresence daemon has not been started"))
		})
	})

	Context("With bad KUBECONFIG", func() {
		It("Reports config error and exits", func() {
			kubeConfig := os.Getenv("KUBECONFIG")
			defer os.Setenv("KUBECONFIG", kubeConfig)
			os.Setenv("KUBECONFIG", "/dev/null")
			stdout, stderr := telepresence()
			Expect(stderr).To(ContainSubstring("kubectl config current-context"))
			Expect(stdout).To(ContainSubstring("Launching Telepresence Daemon"))
			Expect(stdout).To(ContainSubstring("Daemon quitting"))
		})
	})

	Context("With bad context", func() {
		It("Reports connect error and exits", func() {
			stdout, stderr := telepresence("--context", "not-likely-to-exist")
			Expect(stderr).To(ContainSubstring(`"not-likely-to-exist" does not exist`))
			Expect(stdout).To(ContainSubstring("Launching Telepresence Daemon"))
			Expect(stdout).To(ContainSubstring("Daemon quitting"))
		})
	})

	Context("When started with a command", func() {
		It("Connects, executes the command, and then exits", func() {
			stdout, stderr := telepresence("--namespace", namespace, "--", client.GetExe(), "--status")
			Expect(stderr).To(BeEmpty())
			Expect(stdout).To(ContainSubstring("Launching Telepresence Daemon"))
			Expect(stdout).To(ContainSubstring("Connected to context"))
			Expect(stdout).To(ContainSubstring("Context:"))
			Expect(stdout).To(MatchRegexp(proxyOnMatch.String()))
			Expect(stdout).To(ContainSubstring("Daemon quitting"))
		})
	})

	Context("When started in the background", func() {
		itCount := int32(0)
		itTotal := int32(0) // To simulate AfterAll. Add one for each added It() test
		BeforeEach(func() {
			// This is a bit annoying, but ginkgo does not provide a context scoped "BeforeAll"
			// Will be fixed in ginkgo 2.0
			if atomic.CompareAndSwapInt32(&itCount, 0, 1) {
				stdout, stderr := telepresence("--namespace", namespace, "--no-wait")
				Expect(stderr).To(BeEmpty())
				Expect(stdout).To(ContainSubstring("Connected to context"))
			} else {
				atomic.AddInt32(&itCount, 1)
			}
		})

		AfterEach(func() {
			// This is a bit annoying, but ginkgo does not provide a context scoped "AfterAll"
			// Will be fixed in ginkgo 2.0
			if atomic.CompareAndSwapInt32(&itCount, itTotal, 0) {
				stdout, stderr := telepresence("--quit")
				Expect(stderr).To(BeEmpty())
				Expect(stdout).To(ContainSubstring("quitting"))
			}
		})

		It("Reports version from daemon", func() {
			stdout, stderr := telepresence("--version")
			Expect(stderr).To(BeEmpty())
			vs := client.DisplayVersion()
			Expect(stdout).To(ContainSubstring(fmt.Sprintf("Client %s", vs)))
			Expect(stdout).To(ContainSubstring(fmt.Sprintf("Daemon %s", vs)))
		})
		itTotal++

		It("Reports status as connected", func() {
			stdout, stderr := telepresence("--status")
			Expect(stderr).To(BeEmpty())
			Expect(stdout).To(ContainSubstring("Context:"))
		})
		itTotal++

		It("Proxies outbound traffic", func() {
			echoReady := make(chan error)
			go func() {
				echoReady <- applyEchoService()
			}()

			// Give outbound interceptor 15 seconds to kick in.
			proxy := false
			for i := 0; i < 30; i++ {
				stdout, stderr := telepresence("--status")
				Expect(stderr).To(BeEmpty())
				if proxy = proxyOnMatch.MatchString(stdout); proxy {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			Expect(proxy).To(BeTrue(), "Timeout waiting for network overrides to establish")

			err := <-echoReady
			Expect(err).NotTo(HaveOccurred())

			out, err := output("curl", "-s", "echo-easy")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Request served by echo-easy-"))
		})
		itTotal++

		It("Proxies inbound traffic with --intercept", func() {
			stdout, stderr := telepresence("--intercept", "echo-easy", "--port", "9000", "--no-wait")
			Expect(stderr).To(BeEmpty())
			Expect(stdout).To(ContainSubstring("Using deployment echo-easy"))
			srv := &http.Server{Addr: ":9000"}

			defer func() {
				err := srv.Shutdown(context.Background())
				Expect(err).ToNot(HaveOccurred())
				stdout, stderr = telepresence("--remove", "echo-easy")
				Expect(stderr).To(BeEmpty())
				Expect(stdout).To(BeEmpty())
			}()

			go func() {
				http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, "hello from intercept at %s", r.URL.Path)
				})
				err := srv.ListenAndServe()
				Expect(err).To(Equal(http.ErrServerClosed))
			}()

			var err error
			for retry := 0; retry < 100; retry++ {
				stdout, err = output("curl", "-s", "echo-easy")
				if err == nil && !strings.Contains(stdout, "served by echo-easy-") {
					break
				}
				// Inbound proxy hasn't kicked in yet
				time.Sleep(50 * time.Millisecond)
			}
			Expect(err).ToNot(HaveOccurred())
			Expect(stdout).To(Equal("hello from intercept at /"))
		})
		itTotal++
	})
})

var _ = BeforeSuite(func() {
	version.Version = testVersion

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer GinkgoRecover()
		defer wg.Done()
		executable, err := buildExecutable(testVersion)
		Expect(err).NotTo(HaveOccurred())
		client.SetExe(executable)
	}()

	_ = os.Remove(client.ConnectorSocketName)
	err := run("sudo", "true")
	Expect(err).ToNot(HaveOccurred(), "acquire privileges")

	registry := dtest.DockerRegistry()
	os.Setenv("KO_DOCKER_REPO", registry)
	os.Setenv("TELEPRESENCE_REGISTRY", registry)

	wg.Add(1)
	go func() {
		defer GinkgoRecover()
		defer wg.Done()
		err := publishManager(testVersion)
		Expect(err).NotTo(HaveOccurred())
	}()

	wg.Add(1)
	go func() {
		defer GinkgoRecover()
		defer wg.Done()

		kubeconfig := dtest.Kubeconfig()
		os.Setenv("DTEST_KUBECONFIG", kubeconfig)
		os.Setenv("KUBECONFIG", kubeconfig)
		err = run("kubectl", "create", "namespace", namespace)
		Expect(err).NotTo(HaveOccurred())
	}()
	wg.Wait()
})

var _ = AfterSuite(func() {
	_ = run("kubectl", "delete", "namespace", namespace)
})

func applyEchoService() error {
	err := run("ko", "apply", "--namespace", namespace, "-f", "k8s/echo-easy.yaml")
	if err != nil {
		return err
	}
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		err = run(
			"kubectl", "--namespace", namespace, "run", "curl-from-cluster", "--rm", "-it",
			"--image=pstauffer/curl", "--restart=Never", "--",
			"curl", "--silent", "--output", "/dev/null",
			"http://echo-easy."+namespace,
		)
		if err == nil {
			return nil
		}
	}
	return errors.New("timed out waiting for echo-easy service")
}

// runError checks if the given err is a *exit.ExitError, and if so, extracts
// Stderr and the ExitCode from it.
func runError(err error) error {
	if ee, ok := err.(*exec.ExitError); ok {
		if len(ee.Stderr) > 0 {
			err = fmt.Errorf("%s, exit code %d", string(ee.Stderr), ee.ExitCode())
		} else {
			err = fmt.Errorf("exit code %d", ee.ExitCode())
		}
	}
	return err
}

func run(args ...string) error {
	return runError(exec.Command(args[0], args[1:]...).Run())
}

func output(args ...string) (string, error) {
	out, err := exec.Command(args[0], args[1:]...).Output()
	return string(out), runError(err)
}

func publishManager(testVersion string) error {
	cmd := exec.Command("ko", "publish", "--local", "./cmd/traffic")
	cmd.Env = append(os.Environ(),
		fmt.Sprintf(`GOFLAGS=-ldflags=-X=github.com/datawire/telepresence2/pkg/version.Version=%s`,
			testVersion))
	out, err := cmd.Output()
	if err != nil {
		return runError(err)
	}
	imageName := strings.TrimSpace(string(out))
	tag := fmt.Sprintf("%s/tel2:%s", dtest.DockerRegistry(), testVersion)
	err = run("docker", "tag", imageName, tag)
	if err != nil {
		return err
	}
	return run("docker", "push", tag)
}

func buildExecutable(testVersion string) (string, error) {
	executable := filepath.Join("build-output", "bin", "/telepresence")
	return executable, run("go", "build", "-ldflags",
		fmt.Sprintf("-X=github.com/datawire/telepresence2/pkg/version.Version=%s", testVersion),
		"-o", executable, "./cmd/telepresence")
}

func getCommand(args ...string) *cobra.Command {
	cmd := cli.Command()
	cmd.SetArgs(args)
	flags := cmd.Flags()

	// Circumvent test flag conflict explained here https://golang.org/doc/go1.13#testing
	flag.Visit(func(f *flag.Flag) {
		flags.AddGoFlag(f)
	})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SilenceErrors = true
	return cmd
}

func trimmed(f func() io.Writer) string {
	if out, ok := f().(*strings.Builder); ok {
		return strings.TrimSpace(out.String())
	}
	return ""
}

// telepresence executes the CLI command in-process
func telepresence(args ...string) (string, string) {
	cmd := getCommand(args...)
	err := cmd.Execute()
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
	}
	return trimmed(cmd.OutOrStdout), trimmed(cmd.ErrOrStderr)
}
