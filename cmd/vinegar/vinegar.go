package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/vinegarhq/vinegar/config"
	"github.com/vinegarhq/vinegar/config/editor"
	"github.com/vinegarhq/vinegar/internal/dirs"
	"github.com/vinegarhq/vinegar/internal/logs"
	"github.com/vinegarhq/vinegar/internal/state"
	"github.com/vinegarhq/vinegar/roblox"
	"github.com/vinegarhq/vinegar/sysinfo"
	"github.com/vinegarhq/vinegar/wine"
)

var BinPrefix string

func usage() {
	fmt.Fprintln(os.Stderr, "usage: vinegar [-config filepath] player|studio [args...]")
	fmt.Fprintln(os.Stderr, "usage: vinegar [-config filepath] exec prog [args...]")
	fmt.Fprintln(os.Stderr, "       vinegar [-config filepath] edit|kill|uninstall|delete|install-webview2|winetricks|sysinfo")
	os.Exit(1)
}

func main() {
	configPath := flag.String("config", filepath.Join(dirs.Config, "config.toml"), "config.toml file which should be used")
	flag.Parse()

	cmd := flag.Arg(0)
	args := flag.Args()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	switch cmd {
	// These commands don't require a configuration
	case "delete", "edit", "uninstall":
		switch cmd {
		case "delete":
			Delete()
		case "edit":
			if err := editor.Edit(*configPath); err != nil {
				log.Fatal(err)
			}
		case "uninstall":
			Uninstall()
		}
	// These commands (except player & studio) don't require a configuration,
	// but they require a wineprefix, hence wineroot of configuration is required.
	case "sysinfo", "player", "studio", "exec", "kill", "install-webview2", "winetricks":
		cfg, err := config.Load(*configPath)
		if err != nil {
			log.Fatal(err)
		}

		pfx := wine.New(dirs.Prefix, os.Stderr)
		// Always ensure its created, wine will complain if the root
		// directory doesnt exist
		if err := os.MkdirAll(dirs.Prefix, 0o755); err != nil {
			log.Fatal(err)
		}

		switch cmd {
		case "sysinfo":
			Sysinfo(&pfx)
		case "exec":
			if len(args) < 2 {
				usage()
			}

			if err := pfx.Wine(args[1], args[2:]...).Run(); err != nil {
				log.Fatal(err)
			}
		case "kill":
			pfx.Kill()
		case "install-webview2":
			if err := InstallWebview2(&pfx); err != nil {
				log.Fatal(err)
			}
		case "winetricks":
			if err := pfx.Winetricks(); err != nil {
				log.Fatal(err)
			}
		case "player", "studio":
			var b Binary

			logFile := logs.File(cmd)
			logOutput := io.MultiWriter(logFile, os.Stderr)
			pfx.Output = logOutput
			log.SetOutput(logOutput)

			defer logFile.Close()

			switch cmd {
			case "player":
				b = NewBinary(roblox.Player, &cfg, &pfx)
			case "studio":
				b = NewBinary(roblox.Studio, &cfg, &pfx)
			}

			go func() {
				if err := b.Splash.Run(); err != nil {
					log.Printf("splash: %s", err)
				}

				// Will tell Run() to immediately kill Roblox, as it handles INT/TERM.
				// Otherwise, it will just with the same appropiate signal.
				syscall.Kill(syscall.Getpid(), syscall.SIGINT)
			}()

			b.Splash.SetDesc(b.Config.Channel)

			errHandler := func(err error) {
				if !cfg.Splash.Enabled || b.Splash.IsClosed() {
					log.Fatal(err)
				}

				log.Println(err)
				b.Splash.LogPath = (logFile.Name())
				b.Splash.Invalidate()
				select {} // wait for window to close
			}

			if _, err := os.Stat(filepath.Join(pfx.Dir(), "drive_c", "windows")); err != nil {
				log.Printf("Initializing wineprefix at %s", pfx.Dir())

				b.Splash.SetMessage("Initializing wineprefix")
				if err := PrefixInit(&pfx); err != nil {
					b.Splash.SetMessage(err.Error())
					errHandler(err)
				}
			}

			if err := b.Setup(); err != nil {
				b.Splash.SetMessage("Failed to setup Roblox")
				errHandler(err)
			}

			if err := b.Run(args[1:]...); err != nil {
				b.Splash.SetMessage("Failed to run Roblox")
				errHandler(err)
			}
		}
	default:
		usage()
	}
}

func PrefixInit(pfx *wine.Prefix) error {
	if err := pfx.Command("wineboot", "-i").Run(); err != nil {
		return err
	}

	return pfx.SetDPI(97)
}

func Uninstall() {
	vers, err := state.Versions()
	if err != nil {
		log.Fatal(err)
	}

	for _, ver := range vers {
		log.Println("Removing version directory", ver)

		err = os.RemoveAll(filepath.Join(dirs.Versions, ver))
		if err != nil {
			log.Fatal(err)
		}
	}

	err = state.ClearApplications()
	if err != nil {
		log.Fatal(err)
	}
}

func Delete() {
	log.Println("Deleting Wineprefix")
	if err := os.RemoveAll(dirs.Prefix); err != nil {
		log.Fatal(err)
	}
}

func Sysinfo(pfx *wine.Prefix) {
	cmd := pfx.Wine("--version")
	cmd.Stdout = nil // required for Output()
	ver, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}

	info := `## System information
* Distro: %s
* Processor: %s
  * Supports AVX: %t
* Kernel: %s
* Wine: %s`

	fmt.Printf(info, sysinfo.Distro, sysinfo.CPU, sysinfo.HasAVX, sysinfo.Kernel, ver)
	if sysinfo.InFlatpak {
		fmt.Println("* Flatpak: [x]")
	}

	fmt.Println("* Cards:")
	for _, c := range sysinfo.Cards {
		fmt.Printf("  * Card %d: %s %s\n", c.Index, c.Driver, c.Path)
	}
}
