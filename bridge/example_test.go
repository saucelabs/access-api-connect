// Compiled but never run — each example would take a real private device. They
// exist to show the order the calls go in.
package bridge_test

import (
	"context"
	"encoding/base64"
	"log"
	"net"
	"os"
	"time"

	"github.com/saucelabs/access-api-connect/bridge"
)

// Example serves an iOS device's usbmuxd on a local socket, which is what a test
// needs to drive a Sauce device with go-ios and no cable.
func Example() {
	ctx := context.Background()

	apiURL, _, err := bridge.ResolveAPIURL(os.Getenv("SAUCE_API_URL"), os.Getenv("SAUCE_REGION"))
	if err != nil {
		log.Fatal(err)
	}
	username, accessKey := os.Getenv("SAUCE_USERNAME"), os.Getenv("SAUCE_ACCESS_KEY")
	authHeader := bridge.BasicAuthHeader(username, accessKey)

	created, err := bridge.CreateSession(ctx, apiURL, authHeader, bridge.SessionRequest{OS: "IOS"})
	if err != nil {
		log.Fatal(err)
	}
	sessionID, _ := created["id"].(string)

	// Even a session that never became usable holds its device until it expires.
	defer func() {
		if _, err := bridge.CloseSession(ctx, apiURL, sessionID, authHeader, false); err != nil {
			log.Printf("close session %s: %v", sessionID, err)
		}
	}()

	session, err := bridge.WaitForActive(ctx, apiURL, sessionID, authHeader)
	if err != nil {
		log.Fatal(err)
	}

	deviceBridge := bridge.NewDeviceBridge(
		bridge.NestedString(session, "links", "vusbUrl"), sessionID, username, accessKey)
	if err := deviceBridge.Connect(ctx); err != nil {
		log.Fatal(err)
	}
	defer deviceBridge.Close()

	// Ordering, not ceremony. The reader must run before any request, or the
	// reply sits unread and the next call times out. The properties must be
	// cached before serving, because the local handler answers ListDevices from
	// that cache — skip it and a client sees an empty list, which looks exactly
	// like a broken tunnel.
	go deviceBridge.RunUntilError(ctx)
	if _, err := deviceBridge.FetchDeviceProperties(ctx, 10*time.Second); err != nil {
		log.Fatal(err)
	}

	// A path of our own, so nothing needs root. go-ios reads
	// USBMUXD_SOCKET_ADDRESS as a unix path without a colon and as
	// tcp://host:port with one, so a TCP listener works here too.
	listener, err := net.Listen("unix", "/tmp/usbmuxd.sock")
	if err != nil {
		log.Fatal(err)
	}
	go bridge.ServeUsbmux(ctx, listener, deviceBridge)

	os.Setenv("USBMUXD_SOCKET_ADDRESS", listener.Addr().String())
	// ios.ListDevices() now reaches the Sauce device.
}

// ExampleServeADB serves an Android device on the port the controller
// integration tests use, so an emulator forwarded with
// `adb forward tcp:15037 tcp:5555` is interchangeable with a Sauce device.
func ExampleServeADB() {
	ctx := context.Background()

	apiURL, _, err := bridge.ResolveAPIURL(os.Getenv("SAUCE_API_URL"), os.Getenv("SAUCE_REGION"))
	if err != nil {
		log.Fatal(err)
	}
	username, accessKey := os.Getenv("SAUCE_USERNAME"), os.Getenv("SAUCE_ACCESS_KEY")
	authHeader := bridge.BasicAuthHeader(username, accessKey)

	created, err := bridge.CreateSession(ctx, apiURL, authHeader, bridge.SessionRequest{OS: "ANDROID"})
	if err != nil {
		log.Fatal(err)
	}
	sessionID, _ := created["id"].(string)
	defer bridge.CloseSession(ctx, apiURL, sessionID, authHeader, false)

	session, err := bridge.WaitForActive(ctx, apiURL, sessionID, authHeader)
	if err != nil {
		log.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:15037")
	if err != nil {
		log.Fatal(err)
	}

	// Bare base64, not what BasicAuthHeader returns: the per-connection bridge
	// prepends "Basic " itself.
	authB64 := base64.StdEncoding.EncodeToString([]byte(username + ":" + accessKey))

	go bridge.ServeADB(ctx, listener,
		bridge.NestedString(session, "links", "adbUrl"), sessionID, authB64)
	// `adb connect 127.0.0.1:15037` now reaches the Sauce device.
}
