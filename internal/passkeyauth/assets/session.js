/* Sign-out button and expired-session handling for pages behind passkeyauth.
   Include with <script defer src="/auth/session.js"></script> and add a
   hidden <button id="sign-out" hidden>Sign out</button> to the page. */
"use strict";
(() => {
  const nativeFetch = window.fetch.bind(window);
  window.fetch = async (...args) => {
    const response = await nativeFetch(...args);
    // Only the session guard's 401 means "signed out"; an app route may use
    // 401 for its own credentials and must not cost the page its state.
    if (response.status === 401 && response.headers.get("X-Passkey-Auth") === "sign-in") {
      const url = new URL(response.url, location.href);
      if (url.origin === location.origin) location.assign("/");
    }
    return response;
  };
  nativeFetch("/auth/api/me").then(async (response) => {
    if (!response.ok) return;
    const { username } = await response.json();
    const button = document.getElementById("sign-out");
    if (!button) return;
    button.title = `Signed in as ${username}`;
    button.hidden = false;
    button.addEventListener("click", async () => {
      await nativeFetch("/auth/api/logout", { method: "POST" });
      location.assign("/");
    });
  });
})();
