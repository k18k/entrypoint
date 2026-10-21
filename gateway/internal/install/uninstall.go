package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

// Uninstall removes the services and programs. Data stays unless purge is set.
func Uninstall(cfg config.Config, purge bool, out io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("uninstall requires root (sudo)")
	}
	_ = exec.Command("systemctl", append([]string{"disable", "--now"}, UnitNames...)...).Run()
	for _, u := range UnitNames {
		_ = os.Remove(filepath.Join(unitDir, u))
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	_ = os.Remove("/usr/local/bin/entrypoint")
	if err := os.RemoveAll(config.OptDir); err != nil {
		return err
	}
	fmt.Fprintln(out, "Removed services and programs.")
	if !purge {
		fmt.Fprintf(out, "Data kept in %s and %s.\n", cfg.DataDir, config.EtcDir)
		return nil
	}
	if err := os.RemoveAll(cfg.DataDir); err != nil {
		return err
	}
	if err := os.RemoveAll(config.EtcDir); err != nil {
		return err
	}
	for _, u := range []string{config.UserDaemon, config.UserCaddy, config.UserFrps} {
		_ = exec.Command("userdel", u).Run()
		_ = exec.Command("groupdel", u).Run()
	}
	fmt.Fprintln(out, "Removed data, configuration and system users.")
	return nil
}
