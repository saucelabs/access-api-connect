# access-api-connect

A small command-line tool that mounts a **local bridge** to an already-active Sauce Labs Real Device Cloud (RDC) Access API session, exposing the remote device to your local tooling exactly as if it were plugged into your laptop.

- For **iOS** sessions it takes over `/var/run/usbmuxd`, so Xcode, Instruments, `xcrun devicectl`, `ideviceinfo`, Appium with XCUITest, and any other `libimobiledevice`-based tool just *see the device*. **macOS and Linux only** — Windows has no `/var/run/usbmuxd` equivalent.
- For **Android** sessions it listens on `127.0.0.1:7001`, so `adb connect localhost:7001` attaches `adb` to the remote device and every standard `adb` workflow works against it (shell, install, logcat, Appium with UiAutomator2, etc.). Works on macOS, Linux, **and Windows**.

It does **not** create or destroy sessions — bring an already-`ACTIVE` `sessionId` from whatever flow already drives session lifecycle for you.

---

## Download a release

Pre-built binaries are attached to every tagged release on GitHub. Pick the archive matching your platform from the [Releases page](https://github.com/saucelabs/access-api-connect/releases):

| Platform | Archive | iOS | Android |
| --- | --- | :---: | :---: |
| macOS, Apple Silicon | `access-api-connect_<version>_darwin_arm64.tar.gz` | ✓ | ✓ |
| macOS, Intel | `access-api-connect_<version>_darwin_amd64.tar.gz` | ✓ | ✓ |
| Linux, x86_64 | `access-api-connect_<version>_linux_amd64.tar.gz` | ✓ | ✓ |
| Linux, ARM64 | `access-api-connect_<version>_linux_arm64.tar.gz` | ✓ | ✓ |
| Windows, x86_64 | `access-api-connect_<version>_windows_amd64.zip` | — | ✓ |

Each archive contains the `access-api-connect` binary, this README, and the LICENSE. A `checksums.txt` file is published alongside the archives — verify before running:

```
sha256sum -c checksums.txt --ignore-missing
```

Unpack and put the binary on your `PATH`:

```
tar -xzf access-api-connect_*.tar.gz
sudo mv access-api-connect /usr/local/bin/
```

On macOS, the first run may be blocked by Gatekeeper because the binary isn't signed yet. Allow it once via **System Settings → Privacy & Security → "Open Anyway"**, or remove the quarantine attribute:

```
xattr -d com.apple.quarantine /usr/local/bin/access-api-connect
```

---

## Usage

The binary needs to know which Sauce Labs region your session lives in, plus your credentials. Everything can be supplied via environment variables or via the matching command-line flags.

| Variable | Flag | Description |
| --- | --- | --- |
| `SAUCE_REGION` | `--region` | Region the session was created in. Accepts `US`, `US_EAST`, or `EU` (case-insensitive). Required unless `--api-url` is set. |
| `SAUCE_API_URL` | `--api-url` | Explicit API base URL. Overrides `--region` when both are set. Use this for any non-standard endpoint. |
| `SAUCE_USERNAME` | `--username` | Your Sauce Labs username. |
| `SAUCE_ACCESS_KEY` | `--access-key` | Your Sauce Labs access key. |

### Region selection

`--region` is the everyday way to point the tool at the right data center; `--api-url` is the escape hatch for one-off URLs.

| Region | Resolves to |
| --- | --- |
| `US` | `https://api.us-west-1.saucelabs.com` |
| `US_EAST` | `https://api.us-east-4.saucelabs.com` |
| `EU` | `https://api.eu-central-1.saucelabs.com` |

Resolution order is:

1. If `--api-url` (or `SAUCE_API_URL`) is set, that value is used as-is.
2. Otherwise `--region` (or `SAUCE_REGION`) is translated to the URL above. The value is case-insensitive.
3. If neither is set, the binary refuses to start and prints which environment variables to set.

If an API URL is supplied alongside a region (via flag or env var, in any combination), the API URL wins and the binary logs a one-line warning at startup naming the ignored region (e.g. `WARN: explicit API URL is set; ignoring region US`). That way a misconfiguration like accidentally exporting `SAUCE_API_URL` alongside `SAUCE_REGION` is visible but not fatal.

Then pass the session id as the single positional argument.

### iOS

iOS is supported on **macOS and Linux** and requires root, because mounting `/var/run/usbmuxd` is a privileged operation. The existing system `usbmuxd` socket is moved aside to `/var/run/usbmuxd.real` for the duration of the run and restored on clean exit. Running an iOS session on Windows exits immediately with an explanatory message — there's no `usbmuxd` equivalent to take over.

```
sudo SAUCE_REGION=US \
     SAUCE_USERNAME=$SAUCE_USERNAME \
     SAUCE_ACCESS_KEY=$SAUCE_ACCESS_KEY \
     access-api-connect <sessionId>
```

(Replace `US` with `US_EAST` or `EU` if your session was created in those regions; or use `--api-url https://...` for a non-standard endpoint.)

You'll see something like:

```
access-api-connect: session : 513899ea-870e-4d20-a5ed-bb6319d706be
access-api-connect: os      : IOS
access-api-connect: ws url  : wss://api.eu-central-1.saucelabs.com/v1/rdc/vusb/forward
access-api-connect: device  : 00008110-000C4849268A801E
access-api-connect: moved /var/run/usbmuxd -> /var/run/usbmuxd.real
access-api-connect: listening on /var/run/usbmuxd

Ready. usbmuxd mounted at /var/run/usbmuxd. Press Ctrl+C to stop.
```

Now any tool that talks to `usbmuxd` will see the remote device. For example, an Appium XCUITest config:

```json
{
  "platformName": "iOS",
  "appium:automationName": "XCUITest",
  "appium:noReset": true,
  "appium:skipDeviceInitialization": true,
  "appium:udid": "00008110-000C4849268A801E"
}
```

Hit **Ctrl+C** when you're done; the original `/var/run/usbmuxd` is restored automatically.

### Android

Android does **not** require root.

```
SAUCE_REGION=US \
SAUCE_USERNAME=$SAUCE_USERNAME \
SAUCE_ACCESS_KEY=$SAUCE_ACCESS_KEY \
access-api-connect <sessionId>
```

Output:

```
access-api-connect: session : 3e2a…
access-api-connect: os      : ANDROID
access-api-connect: ws url  : wss://api.us-west-1.saucelabs.com/v1/rdc/vusb/forward
access-api-connect: listening on 127.0.0.1:7001

Ready. ADB bridge on 127.0.0.1:7001. Run `adb connect localhost:7001` to attach. Press Ctrl+C to stop.
```

In another terminal:

```
adb connect localhost:7001
adb devices
adb shell
appium --allow-insecure chromedriver_autodownload
```

Appium UiAutomator2 capabilities work as usual; no special `udid` is required.

---

## Flags reference

| Flag | Default | Description |
| --- | --- | --- |
| `--region` | `$SAUCE_REGION` | Sauce Labs region: `US`, `US_EAST`, or `EU` (case-insensitive). Resolves to the matching API URL. |
| `--api-url` | `$SAUCE_API_URL` | Sauce Labs REST API base URL. Overrides `--region` when set. |
| `--username` | `$SAUCE_USERNAME` | Sauce Labs username. |
| `--access-key` | `$SAUCE_ACCESS_KEY` | Sauce Labs access key. |
| `-v`, `--verbose` | off | Print one line per local connection accepted (e.g. `local client #3 connected`). Helpful when debugging Appium or Xcode hangs. Off by default to keep the normal log quiet. |
| `--version` | | Print version (set by GoReleaser) and exit. |

---

## Troubleshooting

**"Low-level access is not available for this session: it is running on a public device"**
Low-level access only works on private devices, so the API omits the `adbUrl` / `usbmuxdUrl` / `vusbUrl` links for sessions allocated to a public device — there is no endpoint for the bridge to attach to. Start a session on a private device from your organization's device pool and re-run against that session id.

**"failed to fetch device properties: timeout waiting for device properties"** *(iOS)*
The WebSocket connected but the remote device never replied with its `ListDevices` answer. Usually means the session has gone unhealthy on the server side. Verify the session is still `ACTIVE` (e.g. via `GET /rdc/v2/sessions/{id}`), then re-run; if it persists, file a support ticket.

**"`/var/run/usbmuxd.real` already exists from a previous run; restore manually"** *(iOS)*
A previous run crashed before it could restore the original socket. Restore it once with `sudo mv /var/run/usbmuxd.real /var/run/usbmuxd` and re-run.

**`adb connect localhost:7001` immediately says "failed to connect"** *(Android)*
The bridge has accepted the TCP connection but the WebSocket upgrade failed — typically a credentials issue. Run with `-v` to confirm the per-connection log line is printed, and double-check `SAUCE_USERNAME` / `SAUCE_ACCESS_KEY` / `SAUCE_API_URL` match the region the session was created in.

**Xcode hangs at "Preparing debug symbols"** *(iOS, first time on a device)*
Xcode downloads the device-support symbols on the first connection per major iOS version. This is normal and can take several minutes. Subsequent runs against the same device are instant.

---

## Building from source

Requires Go 1.22 or newer.

```
git clone https://github.com/saucelabs/access-api-connect
cd access-api-connect
make build      # produces ./access-api-connect for your current OS/arch
make test       # runs the unit tests
make cross      # cross-compiles to dist/access-api-connect-{darwin,linux}-{amd64,arm64}
                # and dist/access-api-connect-windows-amd64.exe
```

A local dry-run of the GoReleaser pipeline (no upload, no tag needed):

```
make release-snapshot
```

The build is pure-Go; Linux artifacts are statically linked (`CGO_ENABLED=0`) and have no glibc or musl dependency.

---

## License

Apache License 2.0 — see [LICENSE](./LICENSE).
