package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/godbus/dbus/v5"
	"github.com/nxadm/tail"
	bsrpc "github.com/vinegarhq/vinegar/bloxstraprpc"
	"github.com/vinegarhq/vinegar/config"
	"github.com/vinegarhq/vinegar/internal/dirs"
	"github.com/vinegarhq/vinegar/internal/state"
	"github.com/vinegarhq/vinegar/roblox"
	boot "github.com/vinegarhq/vinegar/roblox/bootstrapper"
	"github.com/vinegarhq/vinegar/splash"
	"github.com/vinegarhq/vinegar/sysinfo"
	"github.com/vinegarhq/vinegar/wine"
)

const timeout = 6 * time.Second

const (
	DialogUseBrowser = "WebView/InternalBrowser is broken, please use the browser for the action that you were doing."
	DialogQuickLogin = "WebView/InternalBrowser is broken, use Quick Log In to authenticate ('Log In With Another Device' button)"
	DialogFailure    = "Vinegar experienced an error:\n%s"
	DialogNoAVX      = "Warning: Your CPU does not support AVX. While some people may be able to run without it, most are not able to. VinegarHQ cannot provide support for your installation. Continue?"
)

type Binary struct {
	// Only initialized in Main
	Splash *splash.Splash

	GlobalState *state.State
	State       *state.Binary

	GlobalConfig *config.Config
	Config       *config.Binary

	Alias  string
	Name   string
	Dir    string
	Prefix *wine.Prefix
	Type   roblox.BinaryType
	Deploy *boot.Deployment

	// Logging
	Auth     bool
	Activity bsrpc.Activity
}

func BinaryPrefixDir(bt roblox.BinaryType) string {
	return filepath.Join(dirs.Prefixes, strings.ToLower(bt.String()))
}

func NewBinary(bt roblox.BinaryType, cfg *config.Config) (*Binary, error) {
	var bcfg *config.Binary
	var bstate *state.Binary

	s, err := state.Load()
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	switch bt {
	case roblox.Player:
		bcfg = &cfg.Player
		bstate = &s.Player
	case roblox.Studio:
		bcfg = &cfg.Studio
		bstate = &s.Studio
	}

	pfx, err := wine.New(BinaryPrefixDir(bt), bcfg.WineRoot)
	if err != nil {
		return nil, fmt.Errorf("new prefix %s: %w", bt, err)
	}

	os.Setenv("GAMEID", "ulwgl-roblox")

	return &Binary{
		Activity: bsrpc.New(),

		GlobalState: &s,
		State:       bstate,

		GlobalConfig: cfg,
		Config:       bcfg,

		Alias:  bt.String(),
		Name:   bt.BinaryName(),
		Type:   bt,
		Prefix: pfx,
	}, nil
}

func (b *Binary) Main(args ...string) error {
	b.Splash = splash.New(&b.GlobalConfig.Splash)
	b.Config.Env.Setenv()

	logFile, err := LogFile(b.Type.String())
	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}
	defer logFile.Close()

	out := io.MultiWriter(os.Stderr, logFile)
	b.Prefix.Stderr = out
	b.Prefix.Stdout = out
	log.SetOutput(out)
	defer func() {
		b.Splash.LogPath = logFile.Name()
	}()

	firstRun := false
	if _, err := os.Stat(filepath.Join(b.Prefix.Dir(), "drive_c", "windows")); err != nil {
		firstRun = true
	}

	if firstRun && !sysinfo.CPU.AVX {
		b.Splash.Dialog(DialogNoAVX, false)
		slog.Warn("Running roblox without AVX, Roblox will most likely fail to run!")
	}

	go func() {
		err := b.Splash.Run()
		if errors.Is(splash.ErrClosed, err) {
			slog.Warn("Splash window closed!")

			// Will tell Run() to immediately kill Roblox, as it handles INT/TERM.
			// Otherwise, it will just with the same appropiate signal.
			syscall.Kill(syscall.Getpid(), syscall.SIGINT)
			return
		}

		// The splash window didn't close cleanly (ErrClosed), an
		// internal error occured.
		if err != nil {
			log.Fatalf("splash: %s", err)
		}
	}()

	// Command-line flag vs wineprefix initialized
	if firstRun || FirstRun {
		slog.Info("Initializing wineprefix", "dir", b.Prefix.Dir())
		b.Splash.SetMessage("Initializing wineprefix")

		var err error
		switch b.Type {
		case roblox.Player:
			err = b.Prefix.Init()
		case roblox.Studio:
			// Studio accepts all DPIs except the default, which is 96.
			// Technically this is 'initializing wineprefix', as SetDPI calls Wine which
			// automatically create the Wineprefix.
			err = b.Prefix.SetDPI(97)
		}

		if err != nil {
			return fmt.Errorf("failed to init %s prefix: %w", b.Type, err)
		}

		if err := b.InstallWebView(); err != nil {
			return fmt.Errorf("failed to install webview: %w", err)
		}
	}

	// Modify and handle the protocol uri channel
	if len(args) == 1 {
		b.HandleProtocolURI(args[0])
	}

	b.Splash.SetDesc(b.Config.Channel)

	if err := b.Setup(); err != nil {
		return fmt.Errorf("failed to setup roblox: %w", err)
	}

	if err := b.Run(args...); err != nil {
		return fmt.Errorf("failed to run roblox: %w", err)
	}

	return nil
}

func (b *Binary) HandleProtocolURI(mime string) {
	uris := strings.Split(mime, "+")
	for _, uri := range uris {
		kv := strings.Split(uri, ":")

		if len(kv) == 2 && kv[0] == "channel" {
			c := kv[1]
			if c == "" {
				continue
			}

			slog.Warn("Roblox has requested a user channel, changing...", "channel", c)
			b.Config.Channel = c
		}
	}
}

func (b *Binary) Run(args ...string) error {
	if b.Config.DiscordRPC {
		if err := b.Activity.Connect(); err != nil {
			slog.Error("Could not connect to Discord RPC", "error", err)
			b.Config.DiscordRPC = false
		} else {
			defer b.Activity.Close()
		}
	}

	// Studio can run in multiple instances, not Player
	if b.GlobalConfig.MultipleInstances && b.Type == roblox.Player {
		slog.Info("Running robloxmutexer")

		mutexer := b.Prefix.Wine(filepath.Join(BinPrefix, "robloxmutexer.exe"))
		if err := mutexer.Start(); err != nil {
			return fmt.Errorf("start robloxmutexer: %w", err)
		}
		go func() {
			if err := mutexer.Wait(); err != nil {
				slog.Error("robloxmutexer returned too early", "error", err)
			}
		}()
	}

	cmd, err := b.Command(args...)
	if err != nil {
		return fmt.Errorf("%s command: %w", b.Type, err)
	}

	// Roblox will keep running if it was sent SIGINT; requiring acting as the signal holder.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-c

		slog.Warn("Recieved signal", "signal", s)

		// Only kill Roblox if it hasn't exited
		if cmd.ProcessState == nil {
			slog.Warn("Killing Roblox", "pid", cmd.Process.Pid)
			// This way, cmd.Run() will return and vinegar (should) exit.
			cmd.Process.Kill()
		}

		// Don't handle INT after it was recieved, this way if another signal was sent,
		// Vinegar will immediately exit.
		signal.Stop(c)
	}()

	slog.Info("Running Binary", "name", b.Name, "cmd", cmd)
	b.Splash.SetMessage("Launching " + b.Alias)

	go func() {
		// Wait for process to start
		for {
			if cmd.Process != nil {
				break
			}
		}

		// If the log file wasn't found, assume failure
		// and don't perform post-launch roblox functions.
		lf, err := RobloxLogFile(b.Prefix)
		if err != nil {
			slog.Error("Failed to find Roblox log file", "error", err.Error())
			return
		}

		b.Splash.Close()

		if b.Config.GameMode {
			b.RegisterGameMode(int32(cmd.Process.Pid))
		}

		// Blocks and tails file forever until roblox is dead, unless
		// if finding the log file had failed.
		b.Tail(lf)
	}()

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("roblox process: %w", err)
	}

	return nil
}

func RobloxLogFile(pfx *wine.Prefix) (string, error) {
	ad, err := pfx.AppDataDir()
	if err != nil {
		return "", fmt.Errorf("get appdata: %w", err)
	}

	dir := filepath.Join(ad, "Local", "Roblox", "logs")

	// This is required due to fsnotify requiring the directory
	// to watch to exist before adding it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create roblox log dir: %w", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return "", fmt.Errorf("make fsnotify watcher: %w", err)
	}
	defer w.Close()

	if err := w.Add(dir); err != nil {
		return "", fmt.Errorf("watch roblox log dir: %w", err)
	}

	t := time.NewTimer(timeout)

	for {
		select {
		case <-t.C:
			return "", fmt.Errorf("roblox log file not found after %s", timeout)
		case e := <-w.Events:
			if e.Has(fsnotify.Create) {
				return e.Name, nil
			}
		case err := <-w.Errors:
			slog.Error("Recieved fsnotify watcher error", "error", err)
		}
	}
}

func (b *Binary) Tail(name string) {
	t, err := tail.TailFile(name, tail.Config{Follow: true})
	if err != nil {
		slog.Error("Could not tail Roblox log file", "error", err)
		return
	}

	for line := range t.Lines {
		fmt.Fprintln(b.Prefix.Stderr, line.Text)

		if b.Config.DiscordRPC {
			if err := b.Activity.HandleRobloxLog(line.Text); err != nil {
				slog.Error("Activity Roblox log handle failed", "error", err)
			}
		}
	}
}

func (b *Binary) Command(args ...string) (*wine.Cmd, error) {
	if strings.HasPrefix(strings.Join(args, " "), "roblox-studio:1") {
		args = []string{"-protocolString", args[0]}
	}

	cmd := b.Prefix.Wine(filepath.Join(b.Dir, b.Type.Executable()), args...)

	launcher := strings.Fields(b.Config.Launcher)
	if len(launcher) >= 1 {
		cmd.Args = append(launcher, cmd.Args...)
		p, err := b.Config.LauncherPath()
		if err != nil {
			return nil, fmt.Errorf("bad launcher: %w", err)
		}
		cmd.Path = p
	}

	return cmd, nil
}

func (b *Binary) RegisterGameMode(pid int32) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		slog.Error("Failed to connect to D-Bus", "error", err)
		return
	}

	desktop := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")

	call := desktop.Call("org.freedesktop.portal.GameMode.RegisterGame", 0, pid)
	if call.Err != nil && !errors.Is(call.Err, dbus.ErrMsgNoObject) {
		slog.Error("Failed to register to GameMode", "error", err)
		return
	}
}
