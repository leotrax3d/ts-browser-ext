let proxyEnabled = false;

// setPopupIcon sets the icon. It takes either a boolean (for online/offline)
// or the base name of the png file.
function setPopupIcon(base) {
  if (typeof base === "boolean") {
    base = base ? "online" : "offline";
  }
  let iconPath = base + ".png";
  console.log("set icon path to: " + iconPath);

  chrome.action.setIcon({ path: iconPath }, () => {
    if (chrome.runtime.lastError) {
      console.error(
        "Error setting icon to " + iconPath + ":",
        chrome.runtime.lastError.message
      );
    }
  });
}

function enableProxy() {
  if (deadPort) {
    console.error("Cannot enable proxy, disconnected from native host");
    return;
  }

  if (lastProxyPort) {
    nmPort.postMessage({ cmd: "get-status" });
  } else {
    nmPort.postMessage({ cmd: "up" });
  }
}

function disableProxy() {
  console.log("disableProxy called");
  if (nmPort && !deadPort) {
    console.log("Sending down command to native host");
    nmPort.postMessage({ cmd: "down" });
  } else {
    console.log(
      "Cannot send down command - nmPort:",
      !!nmPort,
      "deadPort:",
      deadPort
    );
  }
  proxyEnabled = false;
  lastProxyPort = 0;
  proxyCredential = null;
  console.log(
    "Proxy disabled, proxyEnabled:",
    proxyEnabled,
    "lastProxyPort:",
    lastProxyPort
  );
}

console.log("starting ts-browser-ext");

let popupPort = null;

chrome.runtime.onConnect.addListener((port) => {
  if (port.name != "popup") {
    return;
  }
  popupPort = port;

  console.log("Popup connected");

  port.onMessage.addListener((msg) => {
    console.log("Message from popup:", msg);
  });

  port.onDisconnect.addListener(() => {
    console.log("Popup disconnected");
    popupPort = null;
  });

  sendPopupStatus();
});

// browserByte returns "C" for Chrome.
// The Firefox copy of this file returns "F".
function browserByte() {
  return "C";
}

function sendPopupStatus() {
  if (deadPort) {
    setPopupIcon("need-install");
    console.log("sendPopupStatus... no nmPort");
    if (!everConnected) {
      // Never reached the backend at all, so it isn't registered yet.
      sendToPopup({
        installCmd:
          "go run github.com/tailscale/ts-browser-ext@main --install=" +
          browserByte() +
          chrome.runtime.id,
      });
    } else {
      // It answered before and is gone now, which is a crash or a failed
      // upgrade, not a missing installation. Telling the user to reinstall
      // here would send them down the wrong path.
      sendToPopup({
        backendGone: { message: portError || "", logPath: lastLogPath },
      });
    }
    return;
  }
  if (versionMismatch) {
    setPopupIcon("need-install");
    sendToPopup({
      versionMismatch: Object.assign({}, versionMismatch, {
        logPath: lastLogPath,
        installCmd:
          "go run github.com/tailscale/ts-browser-ext@main --install=" +
          browserByte() +
          chrome.runtime.id,
      }),
    });
    return;
  }
  if (backendError) {
    sendToPopup({
      backendError: { message: backendError, logPath: lastLogPath },
    });
    return;
  }
  setPopupIcon(proxyEnabled ? "online" : "offline");

  sendToPopup({ status: lastStatus });
}

// redactedMessage renders a backend message for the console with the proxy
// password removed. The console is one copy-and-paste away from a bug report,
// and the password is the one field in these messages that must not travel.
function redactedMessage(message) {
  if (!message || !message.procRunning || !message.procRunning.proxyPassword) {
    return JSON.stringify(message);
  }
  const copy = Object.assign({}, message, {
    procRunning: Object.assign({}, message.procRunning, {
      proxyPassword: "[redacted]",
    }),
  });
  return JSON.stringify(copy);
}

function sendToPopup(v) {
  if (popupPort) {
    popupPort.postMessage(v);
  }
}

let nmPort = null; // even non-null if lacking permission
let deadPort = true;
let portError = null;

// everConnected tells "never registered" apart from "was there and stopped".
// The browser's own disconnect message isn't dependable enough to distinguish
// them, and the two need opposite advice.
let everConnected = false;
let lastLogPath = ""; // where the backend says it is writing its log
let proxyCredential = null; // what the backend requires to use its proxy
let backendError = ""; // last error the backend reported, if any
let versionMismatch = null; // set when the backend speaks a different protocol

// PROTOCOL_VERSION is the native messaging protocol this extension speaks.
// The two halves update separately -- this one through the browser's store,
// the backend by re-running --install -- so they drift apart. Comparing
// versions turns that into a message the user can act on, rather than
// commands that quietly do nothing.
const PROTOCOL_VERSION = 1;

connectToNativeHost();

function connectToNativeHost() {
  if (nmPort && !deadPort) {
    return;
  }
  console.log("Connecting to native messaging host...");
  nmPort = chrome.runtime.connectNative("com.tailscale.browserext.chrome");

  nmPort.onDisconnect.addListener(() => {
    deadPort = true;
    setPopupIcon("need-install");
    disableProxy();
    const error = chrome.runtime.lastError;
    if (error) {
      console.error("Connection failed:", error.message);
      portError = error.message;
      setTimeout(connectToNativeHost, 1000);
    } else {
      console.error("Disconnected from native host");
    }
  });
  nmPort.onMessage.addListener((message) => {
    console.log("got message: " + redactedMessage(message));
    if (deadPort) {
      console.log("connected to native backend");
      deadPort = false;
    }
    everConnected = true;
    if (message.procRunning) {
      if (message.procRunning.logPath) {
        lastLogPath = message.procRunning.logPath;
      }

      // A backend too old to report a version predates this check, so treat a
      // missing field as version 0 rather than as agreement.
      const theirs = message.procRunning.protocolVersion || 0;
      if (theirs !== PROTOCOL_VERSION) {
        versionMismatch = {
          theirs: theirs,
          ours: PROTOCOL_VERSION,
          backendVersion: message.procRunning.version || "",
        };
        console.log(
          "protocol mismatch: backend speaks " +
            theirs +
            ", extension speaks " +
            PROTOCOL_VERSION
        );
        // Don't proxy through a backend whose messages we may misread.
        disableProxy();
        sendPopupStatus();
        return;
      }
      versionMismatch = null;

      if (message.procRunning.port) {
        backendError = "";
        // Keep this before setProxy: once the proxy is in use the browser may
        // be challenged immediately, and the listener below needs it.
        proxyCredential = {
          username: message.procRunning.proxyUsername || "",
          password: message.procRunning.proxyPassword || "",
        };
        setProxy(message.procRunning.port);
      } else if (message.procRunning.error) {
        backendError = message.procRunning.error;
        console.log(
          "procRunning error from backend: " + message.procRunning.error
        );
        disableProxy();
      }
    }
    if (message.init && message.init.error) {
      backendError = message.init.error;
      console.log("init error from backend: " + message.init.error);
      disableProxy();
    }
    if (message.status) {
      lastStatus = message.status;
    }
    maybeSendInit();
    sendPopupStatus();
  });
}

var lastProxyPort = 0;
var lastStatus = {}; // last Go status

function setProxy(proxyPort) {
  if (proxyPort) {
    proxyEnabled = true;
    lastProxyPort = proxyPort;
    console.log("Enabling proxy at port: " + proxyPort);
  } else {
    proxyEnabled = false;
    console.log("Disabling proxy...");
    chrome.proxy.settings.set(
      {
        value: {
          mode: "direct",
        },
        scope: "regular",
      },
      () => {
        console.log("Proxy disabled.");
      }
    );
    return;
  }
  chrome.proxy.settings.set(
    {
      value: {
        mode: "fixed_servers",
        rules: {
          singleProxy: {
            scheme: "http",
            host: "127.0.0.1",
            port: proxyPort,
          },
          bypassList: ["localhost", "127.*"],
        },
      },
      scope: "regular",
    },
    () => {
      console.log("Proxy enabled: 127.0.0.1:" + proxyPort);
    }
  );
}

var profileID = "";
var didInit = false;

function maybeSendInit() {
  if (!profileID || didInit || deadPort) {
    return;
  }
  nmPort.postMessage({ cmd: "init", initID: profileID });
  didInit = true;
}

chrome.storage.local.get("profileId", (result) => {
  if (!result.profileId) {
    const profileId = crypto.randomUUID();
    chrome.storage.local.set({ profileId }, () => {
      console.log("Generated profile ID:", profileId);
      profileID = profileId;
      maybeSendInit();
    });
  } else {
    console.log("Profile ID already exists:", result.profileId);
    profileID = result.profileId;
    maybeSendInit();
  }
});

// The backend's proxy demands a credential, so that no other process on this
// machine can use it to reach the tailnet. Supply it when challenged, and
// only to the backend's own proxy: onAuthRequired also fires for servers and
// for proxies that are not ours, and handing the credential to any of those
// would give away the thing it protects.
chrome.webRequest.onAuthRequired.addListener(
  (details) => {
    if (!details.isProxy) {
      return {}; // a website asking for a password; not ours to answer
    }
    if (!proxyCredential || !lastProxyPort) {
      return {};
    }
    if (details.challenger && details.challenger.port !== lastProxyPort) {
      return {};
    }
    return { authCredentials: proxyCredential };
  },
  { urls: ["<all_urls>"] },
  ["blocking"]
);

// Listener for messages from the popup
chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  console.log("bg: Received message:", message);
  if (message.command === "toggleProxy") {
    console.log("bg: toggleProxy received, current proxy=" + proxyEnabled);
    proxyEnabled = !proxyEnabled;
    if (proxyEnabled) {
      console.log("bg: Enabling proxy");
      enableProxy();
      console.log("bg: toggleProxy on, now proxy=" + proxyEnabled);
      sendResponse({ status: lastStatus });
      console.log("bg: toggleProxy on, sent status response");
    } else {
      console.log("bg: Disabling proxy");
      disableProxy();
      console.log("bg: toggleProxy off, now proxy=" + proxyEnabled);
      sendResponse({ status: "Disconnected" });
      console.log("bg: toggleProxy off, sent disconnected response");
    }
    setPopupIcon(proxyEnabled);
    return true; // Keep the message channel open for the async response
  }
});
