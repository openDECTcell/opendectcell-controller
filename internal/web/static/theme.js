// SPDX-License-Identifier: AGPL-3.0-or-later
// Sets the colour theme before the page paints: the choice stored in this
// browser, else the system's. No data leaves the browser.
(function () {
  var t = null;
  try { t = localStorage.getItem("opendectcell-theme"); } catch (e) { /* storage blocked */ }
  if (t !== "light" && t !== "dark") {
    t = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
  document.documentElement.setAttribute("data-bs-theme", t);
})();
