// access-api-connect mounts a local bridge to an already-active Sauce
// Labs RDC Access API session, using the session's multiplex-capable
// WebSocket endpoint for low-level device access.
//
//   - IOS     — takes over /var/run/usbmuxd (root required). PLIST traffic
//     is partially answered locally; Connect / ReadPairRecord /
//     ReadBUID switch their socket into raw passthrough on a
//     fresh multiplex channel.
//   - ANDROID — listens on 127.0.0.1:7001 and pipes each accepted TCP
//     connection straight to its own WebSocket as raw
//     ADB-protocol bytes. Run `adb connect localhost:7001` to
//     attach.
//
// Session creation is NOT performed here. Bring an already-ACTIVE session
// id from your upstream flow.
//
// This file is the CLI only. The reusable half lives in ./bridge, which
// takes a net.Listener so an importing program can choose how the device is
// exposed locally — the CLI's choices below are just one such choice.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/saucelabs/access-api-connect/bridge"
)

// iosSupportError returns a non-nil error explaining why iOS cannot be
// served on the given goos. Currently only Windows is unsupported (it has
// no /var/run/usbmuxd equivalent). Extracted as a function so the
// rationale can be unit-tested without spawning a separate process.
func iosSupportError(goos string) error {
	if goos == "windows" {
		return errors.New(
			"iOS sessions are not supported on Windows: there is no /var/run/usbmuxd equivalent to mount. " +
				"Use a macOS or Linux host for iOS, or run an Android session from this Windows machine.",
		)
	}
	return nil
}

const (
	usbmuxdSocket       = "/var/run/usbmuxd"
	usbmuxdSocketBackup = "/var/run/usbmuxd.real"
	adbPort             = 7001
	adbBindHost         = "127.0.0.1"
)

// Version metadata stamped at build time by GoReleaser (see
// .goreleaser.yaml). Defaults are useful for plain `go build`.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// ---------------------------------------------------------------------------
// usbmuxd socket take-over (iOS path)
// ---------------------------------------------------------------------------

func backupSocket() error {
	if _, err := os.Lstat(usbmuxdSocketBackup); err == nil {
		return fmt.Errorf("%s already exists from a previous run; restore manually: sudo mv %s %s",
			usbmuxdSocketBackup, usbmuxdSocketBackup, usbmuxdSocket)
	}
	if _, err := os.Lstat(usbmuxdSocket); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("%s does not exist (no local usbmuxd running)", usbmuxdSocket)
			return nil
		}
		return err
	}
	if err := os.Rename(usbmuxdSocket, usbmuxdSocketBackup); err != nil {
		return err
	}
	log.Printf("moved %s -> %s", usbmuxdSocket, usbmuxdSocketBackup)
	return nil
}

func restoreSocket() {
	if _, err := os.Lstat(usbmuxdSocket); err == nil {
		if err := os.Remove(usbmuxdSocket); err != nil {
			log.Printf("could not remove %s: %v", usbmuxdSocket, err)
		}
	}
	if _, err := os.Lstat(usbmuxdSocketBackup); err == nil {
		if err := os.Rename(usbmuxdSocketBackup, usbmuxdSocket); err != nil {
			log.Printf("could not restore %s: %v", usbmuxdSocket, err)
		} else {
			log.Printf("restored %s", usbmuxdSocket)
		}
	}
}

// listenUsbmuxUnix binds the unix socket the CLI serves usbmux on. 0666 so
// local clients running as the developer's own user can still reach it after
// we bound it as root.
func listenUsbmuxUnix(socketPath string) (net.Listener, error) {
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o666); err != nil {
		log.Printf("could not chmod %s: %v", socketPath, err)
	}
	return listener, nil
}

// ---------------------------------------------------------------------------
// Run loops
// ---------------------------------------------------------------------------

func runIOS(ctx context.Context, deviceBridge *bridge.DeviceBridge, apiURL, sessionID, username, accessKey string) error {
	if err := iosSupportError(runtime.GOOS); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("mounting %s requires root. Re-run with sudo:\n"+
			"  sudo SAUCE_REGION=$SAUCE_REGION SAUCE_USERNAME=$SAUCE_USERNAME SAUCE_ACCESS_KEY=$SAUCE_ACCESS_KEY %s <sessionId>",
			usbmuxdSocket, os.Args[0])
	}
	authHeader := bridge.BasicAuthHeader(username, accessKey)

	// The reader loop must be running before we issue any request —
	// otherwise the server's response sits in the socket buffer with
	// nothing consuming it and FetchDeviceProperties times out.
	readerErrCh := make(chan error, 1)
	go func() { readerErrCh <- deviceBridge.RunUntilError(ctx) }()

	properties, err := deviceBridge.FetchDeviceProperties(ctx, 10*time.Second)
	if err != nil {
		return fmt.Errorf("failed to fetch device properties: %w", err)
	}
	serial, _ := properties["SerialNumber"].(string)
	if pt, ok := properties["ProductType"].(string); ok && pt != "" {
		log.Printf("device  : %s (%s)", serial, pt)
	} else {
		log.Printf("device  : %s", serial)
	}

	if err := backupSocket(); err != nil {
		return err
	}
	defer restoreSocket()

	listener, err := listenUsbmuxUnix(usbmuxdSocket)
	if err != nil {
		return fmt.Errorf("unix server: %w", err)
	}

	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- bridge.ServeUsbmux(ctx, listener, deviceBridge) }()

	fmt.Printf("\nReady. usbmuxd mounted at %s. Press Ctrl+C to stop.\n\n", usbmuxdSocket)

	// Service the connection until Ctrl+C. A dead WebSocket is not fatal:
	// the usbmuxd mount stays put, in-flight channels are dropped (tools
	// see a device blip), and we redial while the session is ACTIVE.
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverErrCh:
			return fmt.Errorf("unix server: %w", err)
		case err := <-readerErrCh:
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("connection lost: %v — reconnecting (usbmuxd stays mounted)", err)
			deviceBridge.ResetForReconnect()
			if err := bridge.ReconnectWithBackoff(ctx, deviceBridge, apiURL, sessionID, authHeader, readerErrCh); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("reconnect: %w", err)
			}
			log.Printf("reconnected — device available again")
		}
	}
}

func runAndroid(ctx context.Context, wsURL, sessionID, username, accessKey string) error {
	authB64 := base64.StdEncoding.EncodeToString([]byte(username + ":" + accessKey))

	// Bind before printing "Ready" so the address is genuinely accepting by
	// the time the user is told to run `adb connect`.
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", adbBindHost, adbPort))
	if err != nil {
		return fmt.Errorf("tcp server: %w", err)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- bridge.ServeRaw(ctx, listener, wsURL, sessionID, authB64)
	}()

	fmt.Printf("\nReady. ADB bridge on %s:%d. Run `adb connect localhost:%d` to attach. Press Ctrl+C to stop.\n\n",
		adbBindHost, adbPort, adbPort)

	select {
	case <-ctx.Done():
		return nil
	case err := <-serverErrCh:
		return fmt.Errorf("tcp server: %w", err)
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [flags] <sessionId>\n\nFlags:\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	region := flag.String("region", os.Getenv("SAUCE_REGION"),
		"Sauce Labs region: US, US_EAST, EU (case-insensitive). Env SAUCE_REGION. "+
			"Used when --api-url is not set.")
	apiURL := flag.String("api-url", os.Getenv("SAUCE_API_URL"),
		"Sauce Labs REST API base URL. Env SAUCE_API_URL. Overrides --region when set.")
	username := flag.String("username", os.Getenv("SAUCE_USERNAME"), "Sauce Labs username (env SAUCE_USERNAME)")
	accessKey := flag.String("access-key", os.Getenv("SAUCE_ACCESS_KEY"), "Sauce Labs access key (env SAUCE_ACCESS_KEY)")
	verboseShort := flag.Bool("v", false, "verbose logging (per-connection log lines)")
	verboseLong := flag.Bool("verbose", false, "verbose logging (per-connection log lines)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = usage
	flag.Parse()
	bridge.SetVerboseLogging(*verboseShort || *verboseLong)

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("access-api-connect: ")

	if *showVersion {
		fmt.Printf("access-api-connect %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	if flag.NArg() < 1 {
		usage()
		os.Exit(1)
	}
	sessionID := flag.Arg(0)

	if *username == "" || *accessKey == "" {
		fmt.Fprintln(os.Stderr, "set SAUCE_USERNAME and SAUCE_ACCESS_KEY")
		os.Exit(1)
	}
	resolvedURL, warning, err := bridge.ResolveAPIURL(*apiURL, *region)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if warning != "" {
		log.Printf("WARN: %s", warning)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	authHeader := bridge.BasicAuthHeader(*username, *accessKey)

	info, err := bridge.WaitForActive(ctx, resolvedURL, sessionID, authHeader)
	if err != nil {
		log.Fatalf("session: %v", err)
	}

	osKind := bridge.NestedString(info, "device", "os")
	if osKind != "IOS" && osKind != "ANDROID" {
		log.Fatalf("unsupported os=%q (expected IOS or ANDROID)", osKind)
	}

	deviceURL := bridge.NestedString(info, "links", "vusbUrl")
	if deviceURL == "" {
		links, _ := json.Marshal(info["links"])
		log.Fatalf("no vusbUrl in session response. Make sure the session was started with low-level access capabilities. Available links: %s", links)
	}

	log.Printf("session : %s", sessionID)
	log.Printf("os      : %s", osKind)
	log.Printf("ws url  : %s", deviceURL)

	if osKind == "IOS" {
		deviceBridge := bridge.NewDeviceBridge(deviceURL, sessionID, *username, *accessKey)
		if err := deviceBridge.Connect(ctx); err != nil {
			log.Fatalf("ws connect: %v", err)
		}
		defer deviceBridge.Close()

		if err := runIOS(ctx, deviceBridge, resolvedURL, sessionID, *username, *accessKey); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("error: %v", err)
			os.Exit(1)
		}
	} else {
		if err := runAndroid(ctx, deviceURL, sessionID, *username, *accessKey); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("error: %v", err)
			os.Exit(1)
		}
	}
}
