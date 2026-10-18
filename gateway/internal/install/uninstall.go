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

// Uninstall usuwa usługi i programy. Dane zostają, chyba że purge.
func Uninstall(cfg config.Config, purge bool, out io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("odinstalowanie wymaga uprawnień root (sudo)")
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
	fmt.Fprintln(out, "Usunięto usługi i programy.")
	if !purge {
		fmt.Fprintf(out, "Dane zostają w %s i %s.\n", cfg.DataDir, config.EtcDir)
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
	fmt.Fprintln(out, "Usunięto dane, konfigurację i użytkowników systemowych.")
	return nil
}
