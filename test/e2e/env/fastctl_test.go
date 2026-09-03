package env

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"
)

type configCaptureRunner struct {
	fakeRunner
	configContent string
}

func (r *configCaptureRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	for i, arg := range args {
		if arg == "-f" && i+1 < len(args) {
			data, err := os.ReadFile(args[i+1])
			if err != nil {
				return nil, err
			}
			r.configContent = string(data)
		}
	}
	return r.fakeRunner.Run(ctx, dir, name, args...)
}

type sequenceRunner struct {
	commands []recordedCommand
	outputs  [][]byte
	errs     []error
}

func (r *sequenceRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, recordedCommand{
		dir:  dir,
		name: name,
		args: append([]string(nil), args...),
	})
	output := []byte(`{"sandbox":{"identity":{"uid":"sb-id","name":"sb-cli"},"applied_generation":2,"runtime":{"state":4},"data_plane":{"state":4},"ready":true},"generation":2}`)
	if len(r.outputs) > 0 {
		output = r.outputs[0]
		r.outputs = r.outputs[1:]
	}
	var err error
	if len(r.errs) > 0 {
		err = r.errs[0]
		r.errs = r.errs[1:]
	}
	return output, err
}

func TestFastctlRunWritesConfigAndInvokesCLI(t *testing.T) {
	runner := &configCaptureRunner{}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
		WithFastctlConfigDir(t.TempDir()),
	)

	_, err := client.Run(context.Background(), "sb-cli", FastctlConfig{
		Image:   "docker.io/library/alpine:latest",
		PoolRef: "pool-a",
		Command: []string{"/bin/sh"},
		Args:    []string{"-c", "echo FSB_OK && sleep 60"},
		Envs: map[string]string{
			"TEST_VAR": "hello",
		},
		WorkingDir: "/tmp",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	assertCommand(t, runner.commands, "/repo/bin/fastctl",
		"--endpoint", "127.0.0.1:19090",
		"--namespace", "tenant-a",
		"run", "sb-cli", "-f", runner.commands[0].args[len(runner.commands[0].args)-1],
	)

	for _, want := range []string{
		"image: docker.io/library/alpine:latest",
		"pool_ref: pool-a",
		"command:",
		"- /bin/sh",
		"args:",
		"- -c",
		"echo FSB_OK && sleep 60",
		"TEST_VAR: hello",
		"working_dir: /tmp",
	} {
		if !strings.Contains(runner.configContent, want) {
			t.Fatalf("config missing %q:\n%s", want, runner.configContent)
		}
	}
}

func TestFastctlRunWhenCapacityAvailableRetriesOnlyNoCandidate(t *testing.T) {
	runner := &sequenceRunner{
		outputs: [][]byte{
			[]byte("rpc error: code = ResourceExhausted desc = no eligible Fastlet for the Sandbox request"),
			[]byte("created successfully"),
		},
		errs: []error{errors.New("exit status 1"), nil},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlConfigDir(t.TempDir()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	output, err := client.runWhenCapacityAvailable(ctx, "sb-cli", FastctlConfig{
		Image: "docker.io/library/alpine:latest", PoolRef: "pool-a",
	}, time.Millisecond)
	if err != nil {
		t.Fatalf("RunWhenCapacityAvailable returned error: %v", err)
	}
	if string(output) != "created successfully" {
		t.Fatalf("output = %q, want successful retry output", output)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("commands = %d, want two attempts", len(runner.commands))
	}
}

func TestFastctlRunWhenCapacityAvailableDoesNotRetryOtherFailures(t *testing.T) {
	runner := &sequenceRunner{
		outputs: [][]byte{[]byte("rpc error: code = InvalidArgument desc = invalid Pool")},
		errs:    []error{errors.New("exit status 1")},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlConfigDir(t.TempDir()),
	)

	_, err := client.runWhenCapacityAvailable(context.Background(), "sb-cli", FastctlConfig{
		Image: "docker.io/library/alpine:latest", PoolRef: "pool-a",
	}, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "InvalidArgument") {
		t.Fatalf("error = %v, want original non-capacity failure", err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("commands = %d, want one attempt", len(runner.commands))
	}
}

func TestFastctlGetAndDeleteInvokeCLI(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "get", "sb-cli", "-o", "json"): `{"sandbox":{"runtime":{"state":4}}}`,
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "delete", "sb-cli"):            "deleted\n",
		},
		errs: map[string]error{},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
	)

	if _, err := client.GetJSON(context.Background(), "sb-cli"); err != nil {
		t.Fatalf("GetJSON returned error: %v", err)
	}
	if err := client.Delete(context.Background(), "sb-cli"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	assertCommand(t, runner.commands, "/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "get", "sb-cli", "-o", "json")
	assertCommand(t, runner.commands, "/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "delete", "sb-cli")
}

func TestFastctlCommandIncludesSandboxProxyEndpoint(t *testing.T) {
	runner := &fakeRunner{outputs: map[string]string{}, errs: map[string]error{}}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlProxyEndpoint("http://127.0.0.1:18080"),
		WithFastctlNamespace("tenant-a"),
	)

	if _, err := client.Command(context.Background(), "opensandbox", "exec", "sb-cli", "--", "true"); err != nil {
		t.Fatalf("Command returned error: %v", err)
	}
	assertCommand(t, runner.commands, "/repo/bin/fastctl",
		"--endpoint", "127.0.0.1:19090",
		"--namespace", "tenant-a",
		"--proxy-endpoint", "http://127.0.0.1:18080",
		"opensandbox", "exec", "sb-cli", "--", "true",
	)
}

func TestFastctlCanOmitEndpointFlags(t *testing.T) {
	runner := &fakeRunner{outputs: map[string]string{}, errs: map[string]error{}}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlProxyEndpoint("http://127.0.0.1:18080"),
		WithFastctlNamespace("tenant-a"),
		WithoutFastctlEndpointFlags(),
	)

	if _, err := client.Command(context.Background(), "opensandbox", "exec", "sb-cli", "--", "true"); err != nil {
		t.Fatalf("Command returned error: %v", err)
	}
	assertCommand(t, runner.commands, "/repo/bin/fastctl",
		"--namespace", "tenant-a",
		"opensandbox", "exec", "sb-cli", "--", "true",
	)
}

func TestFastctlGetJSONIgnoresCLIConfigPreamble(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "get", "sb-cli", "-o", "json"): "Using config file: /repo/.fastctl/config.json\n{\"sandbox\":{\"identity\":{\"name\":\"sb-cli\"},\"runtime\":{\"state\":4}}}\n",
		},
		errs: map[string]error{},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
	)

	info, err := client.GetJSON(context.Background(), "sb-cli")
	if err != nil {
		t.Fatalf("GetJSON returned error: %v", err)
	}
	if info.GetSandbox().GetIdentity().GetName() != "sb-cli" || info.GetSandbox().GetRuntime().GetState() != fastpathv2.RuntimeState_RUNTIME_STATE_READY {
		t.Fatalf("info = %+v, want Sandbox name sb-cli and Runtime Ready", info)
	}
}

func TestFastctlWaitRunningRequiresAggregateReadyGenerationAndUID(t *testing.T) {
	runner := &sequenceRunner{
		outputs: [][]byte{
			[]byte(`{"sandbox":{"identity":{"uid":"sb-id"},"applied_generation":1,"runtime":{"state":4},"data_plane":{"state":4}},"generation":2}`),
			[]byte(`{"sandbox":{"identity":{"uid":"sb-id"},"applied_generation":2,"runtime":{"state":4},"data_plane":{"state":4},"ready":true},"generation":2}`),
		},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info, err := client.WaitRunning(ctx, "sb-cli")
	if err != nil {
		t.Fatalf("WaitRunning returned error: %v", err)
	}
	if info.GetSandbox().GetIdentity().GetUid() != "sb-id" || info.GetSandbox().GetAppliedGeneration() != info.GetGeneration() {
		t.Fatalf("info = %+v, want Sandbox UID and applied generation", info)
	}
	if len(runner.commands) < 2 {
		t.Fatalf("expected WaitRunning to keep polling until sandbox ID and fastlet pod are set")
	}
}

func TestFastctlUpdateMetadataAndResetInvokeCLI(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string]string{
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "update", "sb-cli", "--metadata", "test=e2e,env=cli"): "updated\n",
			commandKey("/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "reset", "sb-cli"):                                    "reset\n",
		},
		errs: map[string]error{},
	}
	client := NewFastctl(
		WithFastctlRunner(runner),
		WithFastctlBinary("/repo/bin/fastctl"),
		WithFastctlRootDir("/repo"),
		WithFastctlEndpoint("127.0.0.1:19090"),
		WithFastctlNamespace("tenant-a"),
	)

	if _, err := client.UpdateMetadata(context.Background(), "sb-cli", "test=e2e", "env=cli"); err != nil {
		t.Fatalf("UpdateMetadata returned error: %v", err)
	}
	if _, err := client.Reset(context.Background(), "sb-cli"); err != nil {
		t.Fatalf("Reset returned error: %v", err)
	}

	assertCommand(t, runner.commands, "/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "update", "sb-cli", "--metadata", "test=e2e,env=cli")
	assertCommand(t, runner.commands, "/repo/bin/fastctl", "--endpoint", "127.0.0.1:19090", "--namespace", "tenant-a", "reset", "sb-cli")
}
