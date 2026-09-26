/* Passkey (WebAuthn) sign-in and enrollment. No build step. */
"use strict";
const $ = (id) => document.getElementById(id);

const toBuffer = (s) =>
  Uint8Array.from(
    atob(
      s
        .replace(/-/g, "+")
        .replace(/_/g, "/")
        .padEnd(Math.ceil(s.length / 4) * 4, "="),
    ),
    (c) => c.charCodeAt(0),
  ).buffer;
const toBase64url = (buf) =>
  btoa(String.fromCharCode(...new Uint8Array(buf)))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");

function status(message, error = false) {
  $("auth-status").textContent = message;
  $("auth-status").classList.toggle("error", error);
}

async function post(path, body) {
  const response = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok)
    throw new Error(data.error || `Request failed (${response.status})`);
  return data;
}

function encodeCredential(credential) {
  const r = credential.response;
  const response = { clientDataJSON: toBase64url(r.clientDataJSON) };
  if (r.attestationObject) {
    response.attestationObject = toBase64url(r.attestationObject);
    response.transports = r.getTransports ? r.getTransports() : [];
  } else {
    response.authenticatorData = toBase64url(r.authenticatorData);
    response.signature = toBase64url(r.signature);
    if (r.userHandle) response.userHandle = toBase64url(r.userHandle);
  }
  return {
    id: credential.id,
    rawId: toBase64url(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment || undefined,
    clientExtensionResults: credential.getClientExtensionResults(),
    response,
  };
}

function passkeyError(err) {
  if (err.name === "NotAllowedError")
    return "Cancelled or timed out. Try again.";
  if (err.name === "InvalidStateError")
    return "This authenticator already has a passkey for this account.";
  if (err.name === "SecurityError") return "Passkeys need https or localhost.";
  return err.message || "Something went wrong.";
}

async function signIn() {
  $("passkey").disabled = true;
  status("Waiting for your passkey…");
  try {
    const { ceremony, options } = await post("/auth/api/login/begin", {});
    const publicKey = options.publicKey;
    publicKey.challenge = toBuffer(publicKey.challenge);
    for (const c of publicKey.allowCredentials || []) c.id = toBuffer(c.id);
    const credential = await navigator.credentials.get({ publicKey });
    await post("/auth/api/login/finish", {
      ceremony,
      credential: encodeCredential(credential),
    });
    status("Signed in.");
    location.reload(); // the signed-out page was served at the requested URL
  } catch (err) {
    status(passkeyError(err), true);
    $("passkey").disabled = false;
  }
}

async function enroll(event, token) {
  event.preventDefault();
  $("passkey").disabled = true;
  status("Follow your browser's prompts…");
  try {
    const { ceremony, options } = await post("/auth/api/enroll/begin", {
      token,
      name: $("passkey-name").value,
    });
    const publicKey = options.publicKey;
    publicKey.challenge = toBuffer(publicKey.challenge);
    publicKey.user.id = toBuffer(publicKey.user.id);
    for (const c of publicKey.excludeCredentials || []) c.id = toBuffer(c.id);
    const credential = await navigator.credentials.create({ publicKey });
    const { username } = await post("/auth/api/enroll/finish", {
      ceremony,
      credential: encodeCredential(credential),
    });
    status("");
    $("done-lead").textContent = `Signed in as ${username}.`;
    $("enroll").dataset.state = "done";
    $("continue").focus();
  } catch (err) {
    status(passkeyError(err), true);
    $("passkey").disabled = false;
  }
}

function init() {
  if (!window.PublicKeyCredential) {
    $("passkey").disabled = true;
    status("This browser doesn't support passkeys.", true);
    return;
  }
  if (document.body.dataset.page === "login") {
    $("passkey").addEventListener("click", signIn);
    return;
  }
  const token = new URLSearchParams(location.hash.slice(1)).get("token");
  $("enroll-form").addEventListener("submit", (event) => enroll(event, token));
  checkLink(token);
}

// The app is named only once the server accepts the link.
async function checkLink(token) {
  $("invalid-reason").textContent = token
    ? "It may have expired or already been used. Ask for a new one."
    : "Part of the link is missing. Copy the whole link and try again.";
  try {
    if (!token) throw new Error("incomplete link");
    const { app, username } = await post("/auth/api/enroll/check", { token });
    document.title = `Create a passkey · ${app}`;
    $("enroll-app").textContent = $("done-app").textContent = app;
    $("account-initial").textContent = username[0];
    $("account-name").textContent = username;
    $("continue").textContent = `Open ${app}`;
    $("enroll").dataset.state = "ready";
  } catch {
    $("enroll").dataset.state = "invalid";
  }
}
init();
