package pairing

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClient returns a shell script standing in for the rport binary. It answers --version and,
// when run, behaves as described by mode: "good" logs a successful connection and keeps running,
// "nostart" exits at once, "noconnect" keeps running without ever connecting.
func fakeClient(version, mode, logFile string) string {
	run := map[string]string{
		"good":      fmt.Sprintf("echo \"$(date) INFO client: Connected to 192.0.2.1:80 within 5ms\" >> %q\nexec sleep 1000", logFile),
		"nostart":   "echo 'config error' >&2\nexit 1",
		"noconnect": "exec sleep 1000",
	}[mode]
	return fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --version ]; then echo \"version %s\"; exit 0; fi\n%s\n", version, run)
}

type supervisorRun struct {
	dir, stage, bin, conf, log string
	status                     map[string]string
	output                     string
	started                    time.Time
}

// runSupervisor installs a working "old" client, stages a new one in the given mode and runs the
// supervisor with a fake service manager that starts the binary in the background.
func runSupervisor(t *testing.T, newMode string, extraEnv ...string) supervisorRun {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	r := supervisorRun{
		dir:   dir,
		stage: filepath.Join(dir, "stage"),
		bin:   filepath.Join(dir, "bin", "rport"),
		conf:  filepath.Join(dir, "rport.conf"),
		log:   filepath.Join(dir, "rport.log"),
	}
	for _, d := range []string{r.stage + "/previous", r.stage + "/new", filepath.Dir(r.bin)} {
		require.NoError(t, os.MkdirAll(d, 0700))
	}
	write := func(path, content string, mode os.FileMode) {
		require.NoError(t, os.WriteFile(path, []byte(content), mode))
	}
	write(r.bin, fakeClient("0.9.14", "good", r.log), 0755)
	write(r.conf, "[client]\n  server = \"old\"\n", 0644)
	// What the update script does before handing over: back up, then edit the live config.
	write(r.stage+"/previous/rport", fakeClient("0.9.14", "good", r.log), 0755)
	write(r.stage+"/previous/rport.conf", "[client]\n  server = \"old\"\n", 0644)
	write(r.conf, "[client]\n  server = \"old\"\n[monitoring]\n  enabled = true\n", 0644)
	write(r.stage+"/new/rport", fakeClient("0.9.31", newMode, r.log), 0755)
	write(r.log, "earlier run: Connected to 192.0.2.1:80 within 3ms\n", 0644)

	pidFile := filepath.Join(dir, "pid")
	svc := filepath.Join(dir, "svc.sh")
	write(svc, fmt.Sprintf(`#!/bin/sh
case "$1" in
  start) nohup %q >/dev/null 2>&1 & echo $! > %q ;;
  stop) [ -f %q ] && kill "$(cat %q)" 2>/dev/null; rm -f %q; true ;;
  status) [ -f %q ] && kill -0 "$(cat %q)" 2>/dev/null ;;
esac
`, r.bin, pidFile, pidFile, pidFile, pidFile, pidFile, pidFile), 0755)
	t.Cleanup(func() { _ = exec.Command(svc, "stop").Run() })
	require.NoError(t, exec.Command(svc, "start").Run())

	supervisor := filepath.Join(dir, "supervise.sh")
	write(supervisor, string(mustRead("templates/linux/update_supervisor.sh")), 0700)
	cmd := exec.Command(sh, supervisor)
	cmd.Env = append(os.Environ(),
		"STAGE="+r.stage, "RPORT_BIN="+r.bin, "CONFIG_FILE="+r.conf, "LOG_FILE="+r.log,
		"FROM_VERSION=0.9.14", "TO_VERSION=0.9.31", "SERVICE_CMD="+svc,
		"HEALTH_TIMEOUT=4", "STABLE_SECS=1", "HANDOVER_DELAY=0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	done := make(chan struct{})
	var out []byte
	r.started = time.Now()
	go func() { out, _ = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("supervisor did not finish")
	}
	r.output = string(out)

	data, err := os.ReadFile(filepath.Join(r.stage, "status.json"))
	require.NoError(t, err, r.output)
	require.NoError(t, json.Unmarshal(data, &r.status), string(data))
	return r
}

func (r supervisorRun) running(t *testing.T) bool {
	t.Helper()
	return exec.Command(filepath.Join(r.dir, "svc.sh"), "status").Run() == nil
}

func (r supervisorRun) installedVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command(r.bin, "--version").Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func TestUpdateSupervisorKeepsAHealthyClient(t *testing.T) {
	r := runSupervisor(t, "good")
	assert.Equal(t, "success", r.status["status"], r.output)
	assert.Equal(t, "0.9.14", r.status["from"])
	assert.Equal(t, "0.9.31", r.status["to"])
	assert.Equal(t, "version 0.9.31", r.installedVersion(t))
	assert.True(t, r.running(t))
	conf, _ := os.ReadFile(r.conf)
	assert.Contains(t, string(conf), "[monitoring]", "the updated config is kept")
}

func TestUpdateSupervisorRollsBackAClientThatDoesNotStart(t *testing.T) {
	r := runSupervisor(t, "nostart")
	assert.Equal(t, "rolled_back", r.status["status"], r.output)
	assert.Contains(t, r.status["message"], "0.9.31")
	assert.Equal(t, "version 0.9.14", r.installedVersion(t))
	assert.True(t, r.running(t), "the previous client runs again")
	conf, _ := os.ReadFile(r.conf)
	assert.Equal(t, "[client]\n  server = \"old\"\n", string(conf), "the previous config is restored")
}

func TestUpdateSupervisorRollsBackAClientThatNeverConnects(t *testing.T) {
	r := runSupervisor(t, "noconnect")
	assert.Equal(t, "rolled_back", r.status["status"], r.output)
	assert.Equal(t, "version 0.9.14", r.installedVersion(t))
	assert.True(t, r.running(t))
	logData, _ := os.ReadFile(filepath.Join(r.stage, "update.log"))
	assert.Contains(t, string(logData), "did not connect", "the log says why")
}

func TestUpdateScriptHandsOverToTheSupervisor(t *testing.T) {
	body, _ := renderUpdate(osLinux, scriptSettings{DownloadsURL: dlURL, PairingURL: publicURL + "/pairing"})
	s := string(body)
	assert.Contains(t, s, "RPORT_SUPERVISOR_EOF", "the supervisor is written out by the update script")
	assert.Contains(t, s, string(mustRead("templates/linux/update_supervisor.sh")))
	assert.Contains(t, s, "systemd-run")

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	out, err := exec.Command(sh, "-n", "-c", s).CombinedOutput()
	assert.NoError(t, err, "the rendered script parses: %s", out)
}

func TestWindowsUpdateScriptHandsOverToTheSupervisor(t *testing.T) {
	body, _ := renderUpdate(osWindows, scriptSettings{DownloadsURL: dlURL, PairingURL: publicURL + "/pairing"})
	s := string(body)
	assert.Contains(t, s, "function Write-UpdateSupervisor")
	assert.Contains(t, s, string(mustRead("templates/windows/update_supervisor.ps1")))
	assert.Contains(t, s, "Register-ScheduledTask")
	assert.Contains(t, s, `previous\rport.exe`, "the current client is backed up")
	// Regression: the staged rport.exe used to be deleted before the restart task could install it.
	assert.NotContains(t, s, "Remove-Item $temp -Recurse -Force\n    # Now the new rport.exe")
}

// The client must get time to report the result of the command that started the update before the
// supervisor stops it; otherwise the job stays "running" in the UI.
func TestUpdateSupervisorWaitsBeforeStoppingTheClient(t *testing.T) {
	r := runSupervisor(t, "good", "HANDOVER_DELAY=2")
	assert.Equal(t, "success", r.status["status"], r.output)
	logData, _ := os.ReadFile(filepath.Join(r.stage, "update.log"))
	assert.Contains(t, string(logData), "waiting 2s")
	assert.GreaterOrEqual(t, time.Since(r.started), 3*time.Second)
}

// rportd only accepts TLS 1.3 by default; scripts that force TLS 1.2 cannot even download.
func TestWindowsScriptsDoNotForceTLS12Only(t *testing.T) {
	update, _ := renderUpdate(osWindows, scriptSettings{DownloadsURL: dlURL, PairingURL: publicURL + "/pairing"})
	installer, _, err := renderInstaller(osWindows, testDeposit, scriptSettings{DownloadsURL: dlURL, PairingURL: publicURL + "/pairing"})
	require.NoError(t, err)
	for name, s := range map[string]string{"update": string(update), "installer": string(installer)} {
		assert.NotContains(t, s, "SecurityProtocol = [Net.SecurityProtocolType]::Tls12", name)
		assert.Contains(t, s, "Tls13", name)
	}
}

// Seen on a Windows Server 2019 host: the scheduled task stays "Queued" and never runs. The update
// script then starts the supervisor through WMI instead.
func TestWindowsUpdateFallsBackWhenTheTaskDoesNotStart(t *testing.T) {
	body, _ := renderUpdate(osWindows, scriptSettings{DownloadsURL: dlURL, PairingURL: publicURL + "/pairing"})
	s := string(body)
	assert.Contains(t, s, "Win32_Process")
	assert.Contains(t, s, "did not start; starting the supervisor through WMI")
}

func TestUpdateSupervisorReportsTheChange(t *testing.T) {
	r := runSupervisor(t, "good", "CHANGE=server to http://connect.procy.sh")
	assert.Equal(t, "success", r.status["status"], r.output)
	assert.Contains(t, r.status["message"], "server to http://connect.procy.sh")
}

func TestUpdateScriptOffersServerSwitch(t *testing.T) {
	body, _ := renderUpdate(osLinux, scriptSettings{DownloadsURL: dlURL, PairingURL: publicURL + "/pairing",
		ConnectURLs: []string{"http://connect.procy.sh", "http://us-central1.connect.procy.sh"}})
	s := string(body)
	assert.Contains(t, s, `RPORT_CONNECT_URLS="http://connect.procy.sh http://us-central1.connect.procy.sh"`)
	assert.Contains(t, s, "switch_server")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	out, err := exec.Command(sh, "-n", "-c", s).CombinedOutput()
	assert.NoError(t, err, "the rendered script parses: %s", out)
}
