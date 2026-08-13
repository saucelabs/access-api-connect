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
	"sync"
	"syscall"
	"time"
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

// verboseLogging is set once in main() before any goroutines spin up, so
// we can read it without a lock thereafter. Gates per-connection log lines.
var verboseLogging bool

func vlog(format string, args ...interface{}) {
	if verboseLogging {
		log.Printf(format, args...)
	}
}

// basicAuthHeader builds an HTTP Basic Authorization header value from raw
// Sauce credentials.
func basicAuthHeader(username, accessKey string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+accessKey))
}

// ---------------------------------------------------------------------------
// Server helpers
// ---------------------------------------------------------------------------

func runUnixServer(ctx context.Context, bridge localBridge, socketPath string) error {
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o666); err != nil {
		log.Printf("could not chmod %s: %v", socketPath, err)
	}
	log.Printf("listening on %s", socketPath)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		go NewLocalUsbmuxHandler(bridge, conn).Run()
	}
}

func runTCPServer(ctx context.Context, wsURL, sessionID, authB64, host string, port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	defer listener.Close()
	log.Printf("listening on %s:%d", host, port)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		bridge := NewAdbConnectionBridge(wsURL, sessionID, authB64, conn)
		wg.Add(1)
		go func() {
			defer wg.Done()
			bridge.Run(ctx)
		}()
	}
}

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

// ---------------------------------------------------------------------------
// Run loops
// ---------------------------------------------------------------------------

func runIOS(ctx context.Context, bridge *DeviceBridge, apiURL, sessionID, username, accessKey string) error {
	if err := iosSupportError(runtime.GOOS); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("mounting %s requires root. Re-run with sudo:\n"+
			"  sudo SAUCE_REGION=$SAUCE_REGION SAUCE_USERNAME=$SAUCE_USERNAME SAUCE_ACCESS_KEY=$SAUCE_ACCESS_KEY %s <sessionId>",
			usbmuxdSocket, os.Args[0])
	}
	authHeader := basicAuthHeader(username, accessKey)

	// The reader loop must be running before we issue any request —
	// otherwise the server's response sits in the socket buffer with
	// nothing consuming it and FetchDeviceProperties times out.
	readerErrCh := make(chan error, 1)
	go func() { readerErrCh <- bridge.RunUntilError(ctx) }()

	properties, err := bridge.FetchDeviceProperties(ctx, 10*time.Second)
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

	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- runUnixServer(ctx, bridge, usbmuxdSocket) }()

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
			bridge.ResetForReconnect()
			if err := reconnect(ctx, bridge, apiURL, sessionID, authHeader, readerErrCh); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("reconnect: %w", err)
			}
			log.Printf("reconnected — device available again")
		}
	}
}

// reconnect redials the session WebSocket with capped backoff (0.5s-5s).
// Each attempt first reads the session state: no longer ACTIVE ends the
// loop with an error (caller exits); a failed read (unreachable API, HTTP
// error, bad body) is transient and retried. On success the reader is
// restarted and a device-properties round trip confirms the tunnel.
func reconnect(ctx context.Context, bridge *DeviceBridge, apiURL, sessionID, authHeader string, readerErrCh chan error) error {
	const maxBackoff = 5 * time.Second
	backoff := 500 * time.Millisecond

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Non-ACTIVE stops the loop; a failed read (err != nil) is transient.
		if info, err := fetchSession(ctx, apiURL, sessionID, authHeader); err == nil {
			if state, _ := info["state"].(string); state != "ACTIVE" {
				return fmt.Errorf("session is no longer active (state=%q)", state)
			}
		}

		if err := bridge.Reconnect(ctx); err != nil {
			log.Printf("reconnect attempt %d failed: %v (next try in %s)", attempt, err, backoff)
		} else {
			go func() { readerErrCh <- bridge.RunUntilError(ctx) }()
			if _, err := bridge.FetchDeviceProperties(ctx, 10*time.Second); err == nil {
				return nil
			} else {
				log.Printf("reconnect attempt %d: tunnel check failed: %v (next try in %s)", attempt, err, backoff)
				_ = bridge.Close()
				<-readerErrCh // reap the reader we just started
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func runAndroid(ctx context.Context, wsURL, sessionID, username, accessKey string) error {
	authB64 := base64.StdEncoding.EncodeToString([]byte(username + ":" + accessKey))
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- runTCPServer(ctx, wsURL, sessionID, authB64, adbBindHost, adbPort)
	}()
	// Let the listener bind before we print "Ready".
	time.Sleep(200 * time.Millisecond)

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
	verboseLogging = *verboseShort || *verboseLong

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
	resolvedURL, warning, err := resolveAPIURL(*apiURL, *region)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if warning != "" {
		log.Printf("WARN: %s", warning)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	authHeader := basicAuthHeader(*username, *accessKey)

	info, err := waitForActive(ctx, resolvedURL, sessionID, authHeader)
	if err != nil {
		log.Fatalf("session: %v", err)
	}

	osKind := nestedString(info, "device", "os")
	if osKind != "IOS" && osKind != "ANDROID" {
		log.Fatalf("unsupported os=%q (expected IOS or ANDROID)", osKind)
	}

	deviceURL := nestedString(info, "links", "vusbUrl")
	if deviceURL == "" {
		links, _ := json.Marshal(info["links"])
		log.Fatalf("no vusbUrl in session response. Make sure the session was started with low-level access capabilities. Available links: %s", links)
	}

	log.Printf("session : %s", sessionID)
	log.Printf("os      : %s", osKind)
	log.Printf("ws url  : %s", deviceURL)

	if osKind == "IOS" {
		bridge := NewDeviceBridge(deviceURL, sessionID, *username, *accessKey)
		if err := bridge.Connect(ctx); err != nil {
			log.Fatalf("ws connect: %v", err)
		}
		defer bridge.Close()

		if err := runIOS(ctx, bridge, resolvedURL, sessionID, *username, *accessKey); err != nil && !errors.Is(err, context.Canceled) {
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
