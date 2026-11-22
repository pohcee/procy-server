package pairing

import (
	"bytes"
	"embed"
	"fmt"
	"net/http"
	"strings"
	"text/template"
)

//go:embed templates
var templates embed.FS

const (
	osLinux   = "linux"
	osWindows = "windows"
)

var (
	linuxVarsTpl   = template.Must(template.ParseFS(templates, "templates/linux/installer_vars.sh"))
	windowsVarsTpl = template.Must(template.ParseFS(templates, "templates/windows/vars.ps1"))
)

// clientOS picks the script flavour by User-Agent, like the standalone service:
// PowerShell (Invoke-WebRequest/Invoke-RestMethod) gets Windows, everything else Linux.
func clientOS(r *http.Request) string {
	if strings.Contains(r.UserAgent(), "PowerShell") {
		return osWindows
	}
	return osLinux
}

// scriptSettings are the values every rendered script gets.
type scriptSettings struct {
	// DownloadsURL is the base URL of the client packages ("" makes the scripts fail clearly).
	DownloadsURL string
	// PairingURL is the public URL of the pairing endpoints, used for client self-updates.
	PairingURL string
}

type script struct {
	buf bytes.Buffer
}

// include writes an embedded file surrounded by begin/end comments.
func (s *script) include(name string) {
	content := mustRead(name)
	fmt.Fprintf(&s.buf, "\n# BEGINNING of %s %s|\n\n", name, strings.Repeat("-", max0(102-len(name))))
	s.buf.Write(content)
	fmt.Fprintf(&s.buf, "\n# END of %s %s|\n\n", name, strings.Repeat("-", max0(108-len(name))))
}

// includeRaw writes an embedded file followed by a new line.
func (s *script) includeRaw(name string) {
	s.buf.Write(mustRead(name))
	s.buf.WriteString("\n")
}

func (s *script) render(name string, tpl *template.Template, data interface{}) error {
	fmt.Fprintf(&s.buf, "## BEGINNING of rendered template %s\n", name)
	if err := tpl.Execute(&s.buf, data); err != nil {
		return fmt.Errorf("executing template %s: %w", name, err)
	}
	fmt.Fprintf(&s.buf, "\n## END of rendered template %s\n\n", name)
	return nil
}

// downloadBase defines where the scripts download the client from and the URL of the
// pairing endpoints the scripts use for self-updates.
func (s *script) downloadBase(os string, cfg scriptSettings) {
	if os == osWindows {
		fmt.Fprintf(&s.buf, "$RportDownloadBase = \"%s\"\n$RportPairingUrl = \"%s\"\n",
			EscapeForPowerShell(cfg.DownloadsURL), EscapeForPowerShell(cfg.PairingURL))
		return
	}
	fmt.Fprintf(&s.buf, "RPORT_DOWNLOAD_BASE=\"%s\"\nRPORT_PAIRING_URL=\"%s\"\n",
		EscapeForBash(cfg.DownloadsURL), EscapeForBash(cfg.PairingURL))
}

func mustRead(name string) []byte {
	b, err := templates.ReadFile(name)
	if err != nil {
		// all names are constants of this package, covered by tests
		panic(fmt.Sprintf("pairing: embedded file %s missing: %v", name, err))
	}
	return b
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// renderInstaller returns the installer script with the deposit embedded and its file name.
func renderInstaller(os string, data Deposit, cfg scriptSettings) ([]byte, string, error) {
	s := &script{}
	if os == osWindows {
		s.includeRaw("templates/windows/installer_init.ps1")
		s.include("templates/header.txt")
		if err := s.render("templates/windows/vars.ps1", windowsVarsTpl, data.escaped(EscapeForPowerShell)); err != nil {
			return nil, "", err
		}
		s.downloadBase(os, cfg)
		s.include("templates/windows/functions.ps1")
		s.include("templates/windows/install.ps1")
		return s.buf.Bytes(), "rport-installer.ps1", nil
	}
	s.includeRaw("templates/linux/init.sh")
	s.include("templates/header.txt")
	if err := s.render("templates/linux/installer_vars.sh", linuxVarsTpl, data.escaped(EscapeForBash)); err != nil {
		return nil, "", err
	}
	s.downloadBase(os, cfg)
	s.include("templates/linux/vars.sh")
	s.include("templates/linux/functions.sh")
	s.include("templates/linux/install.sh")
	return s.buf.Bytes(), "rport-installer.sh", nil
}

// renderUpdate returns the client update script and its file name. It holds no credentials.
func renderUpdate(os string, cfg scriptSettings) ([]byte, string) {
	s := &script{}
	if os == osWindows {
		s.includeRaw("templates/windows/update_init.ps1")
		s.include("templates/header.txt")
		s.downloadBase(os, cfg)
		s.include("templates/windows/functions.ps1")
		s.include("templates/windows/update.ps1")
		return s.buf.Bytes(), "rport-update.ps1"
	}
	s.includeRaw("templates/linux/init.sh")
	s.include("templates/header.txt")
	s.downloadBase(os, cfg)
	s.include("templates/linux/vars.sh")
	s.include("templates/linux/functions.sh")
	s.include("templates/linux/update.sh")
	return s.buf.Bytes(), "rport-update.sh"
}
