// SPDX-License-Identifier: AGPL-3.0-or-later
// The few behaviours the pages need. Everything works without it except the
// theme switch and the map.
(function () {
  "use strict";
  var root = document.documentElement;

  document.querySelectorAll("[data-theme-toggle]").forEach(function (b) {
    b.addEventListener("click", function () {
      var next = root.getAttribute("data-bs-theme") === "dark" ? "light" : "dark";
      root.setAttribute("data-bs-theme", next);
      try { localStorage.setItem("opendectcell-theme", next); } catch (e) { /* storage blocked */ }
    });
  });

  // confirm="text" on a form: ask before a destructive post
  document.querySelectorAll("form[data-confirm]").forEach(function (f) {
    f.addEventListener("submit", function (e) {
      if (!window.confirm(f.getAttribute("data-confirm"))) { e.preventDefault(); }
    });
  });

  // a chart's day: its text next to the pointer (data-tip on the group)
  var tip = null;
  document.querySelectorAll("svg.chart").forEach(function (svg) {
    var hide = function () { if (tip) { tip.hidden = true; } };
    svg.addEventListener("mousemove", function (e) {
      var g = e.target.closest ? e.target.closest("g[data-tip]") : null;
      if (!g) { hide(); return; }
      if (!tip) {
        tip = document.createElement("div");
        tip.className = "chart-tip";
        document.body.appendChild(tip);
      }
      tip.textContent = g.getAttribute("data-tip");
      tip.hidden = false;
      var x = Math.min(e.clientX + 14, window.innerWidth - tip.offsetWidth - 8);
      tip.style.left = Math.max(8, x) + "px";
      tip.style.top = (e.clientY + 18) + "px";
    });
    svg.addEventListener("mouseleave", hide);
  });

  // form[data-dirty]: the submit button waits for a change, so "Save"
  // cannot be pressed on what is already stored
  document.querySelectorAll("form[data-dirty]").forEach(function (f) {
    var snap = function () { return new URLSearchParams(new FormData(f)).toString(); };
    var start = snap();
    var btns = f.querySelectorAll("button[type=submit]");
    var sync = function () { btns.forEach(function (b) { b.disabled = snap() === start; }); };
    f.addEventListener("input", sync);
    f.addEventListener("change", sync);
    window.addEventListener("pageshow", function () { start = snap(); sync(); });
    sync();
  });

  // The VoIP line form: its tabs, and the fields that only matter when
  // another is set (data-dep="field:value,value", a box being on or off).
  // Hidden fields are still sent, so what was typed there is kept.
  document.querySelectorAll("form.sip-tabs").forEach(function (f) {
    f.classList.add("tabs-on");
    var links = f.querySelectorAll("[data-sip-tab]");
    var show = function (id) {
      links.forEach(function (a) { a.classList.toggle("active", a.getAttribute("data-sip-tab") === id); });
      f.querySelectorAll(".sip-pane").forEach(function (p) { p.classList.toggle("active", p.id === "tab-" + id); });
    };
    links.forEach(function (a) {
      a.addEventListener("click", function (e) { e.preventDefault(); show(a.getAttribute("data-sip-tab")); });
    });
    // a field the browser refuses sits on a tab out of view: open it
    f.addEventListener("invalid", function (e) {
      var p = e.target.closest(".sip-pane");
      if (p) { show(p.id.slice(4)); }
    }, true);
    var deps = f.querySelectorAll("[data-dep]");
    var sync = function () {
      deps.forEach(function (d) {
        var parts = d.getAttribute("data-dep").split(":");
        var c = f.elements[parts[0]];
        if (!c) { return; }
        var v = c.type === "checkbox" ? (c.checked ? "on" : "off") : c.value;
        d.hidden = parts[1].split(",").indexOf(v) < 0;
      });
    };
    f.addEventListener("change", sync);
    sync();
  });

  // the sidebar keeps its scroll position from page to page: every menu
  // choice is a page load, and it used to start at the top again
  var side = document.getElementById("sidebar-menu");
  var navs = [side, document.querySelector("aside.navbar-vertical")];
  var saved = null;
  try { saved = window.sessionStorage.getItem("opendectcell-nav-scroll"); } catch (e) { /* storage blocked */ }
  if (saved !== null) { navs.forEach(function (n) { if (n) { n.scrollTop = parseInt(saved, 10) || 0; } }); }
  var keep = function () {
    var top = 0;
    navs.forEach(function (n) { if (n && n.scrollTop > top) { top = n.scrollTop; } });
    try { window.sessionStorage.setItem("opendectcell-nav-scroll", String(top)); } catch (e) { /* storage blocked */ }
  };
  if (side) { side.addEventListener("click", keep); }
  window.addEventListener("pagehide", keep);

  // The tile layers send the page's origin as the Referer: the site's own
  // Referrer-Policy is same-origin, and OpenStreetMap's tile servers refuse
  // requests without a Referer (their tile usage policy).

  // the position picker of a base station's page
  var mapEl = document.getElementById("station-map");
  var lat = document.getElementById("lat"), lon = document.getElementById("lon");
  if (mapEl && window.L && lat && lon) {
    var tiles = mapEl.getAttribute("data-tiles");
    var has = lat.value !== "" && lon.value !== "";
    var start = has ? [parseFloat(lat.value), parseFloat(lon.value)] : [51, 10];
    var map = window.L.map(mapEl, { attributionControl: !!tiles }).setView(start, has ? 14 : 4);
    if (tiles) {
      window.L.tileLayer(tiles, { maxZoom: 19, referrerPolicy: "origin", attribution: mapEl.getAttribute("data-attribution") || "" }).addTo(map);
    } else {
      mapEl.classList.add("station-map-blank");
    }
    var marker = null;
    var put = function (ll, fromMap) {
      if (!marker) {
        marker = window.L.marker(ll, { draggable: true }).addTo(map);
        marker.on("dragend", function () { put(marker.getLatLng(), true); });
      } else {
        marker.setLatLng(ll);
      }
      if (fromMap) {
        lat.value = ll.lat.toFixed(5);
        lon.value = ll.lng.toFixed(5);
      }
    };
    if (has) { put(window.L.latLng(start[0], start[1]), false); }
    map.on("click", function (e) { put(e.latlng, true); });
    var typed = function () {
      var a = parseFloat(lat.value.replace(",", ".")), b = parseFloat(lon.value.replace(",", "."));
      if (isFinite(a) && isFinite(b) && Math.abs(a) <= 90 && Math.abs(b) <= 180) {
        put(window.L.latLng(a, b), false);
        map.setView([a, b], Math.max(map.getZoom(), 12));
      }
    };
    lat.addEventListener("change", typed);
    lon.addEventListener("change", typed);
  }

  // the map of shared stations
  var pub = document.getElementById("public-map");
  if (pub && window.L) {
    var ptiles = pub.getAttribute("data-tiles");
    var pm = window.L.map(pub, { attributionControl: !!ptiles }).setView([51, 10], 4);
    if (ptiles) {
      window.L.tileLayer(ptiles, { maxZoom: 19, referrerPolicy: "origin", attribution: pub.getAttribute("data-attribution") || "" }).addTo(pm);
    } else {
      pub.classList.add("station-map-blank");
    }
    window.fetch(pub.getAttribute("data-feed"), { credentials: "same-origin" })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        var pts = [];
        (d.stations || []).forEach(function (st) {
          if (st.lat === undefined || st.lon === undefined) { return; }
          var m = window.L.marker([st.lat, st.lon]).addTo(pm);
          var box = document.createElement("div");
          var title = document.createElement("strong");
          title.textContent = st.name || "Base station";
          box.appendChild(title);
          var line = function (t) { var p = document.createElement("div"); p.textContent = t; box.appendChild(p); };
          if (st.model) { line(st.model); }
          if (st.stats) {
            line(st.stats.handsets + " handsets, " + st.stats.calls_7d + " calls in 7 days");
            line("talk time " + Math.round(st.stats.talk_s_7d / 60) + " min, up " + Math.round(st.stats.uptime_s / 3600) + " h");
          }
          m.bindPopup(box);
          pts.push([st.lat, st.lon]);
        });
        if (pts.length) { pm.fitBounds(pts, { maxZoom: 12, padding: [30, 30] }); }
        else { document.getElementById("public-map-empty").hidden = false; }
      })
      .catch(function () { /* the map stays empty */ });
  }

  // registering a handset: open a window through the API, show the PIN once,
  // wait for the handset
  var reg = document.getElementById("reg");
  if (reg) {
    var $ = function (id) { return document.getElementById(id); };
    var win = null, timer = null;
    var show = function (name) {
      ["reg-start", "reg-open", "reg-done"].forEach(function (n) { $(n).hidden = n !== name; });
      var at = { "reg-start": 1, "reg-open": 2, "reg-done": 3 }[name];
      [1, 2, 3].forEach(function (k) {
        var li = $("step-" + k);
        li.classList.toggle("active", k === at);
      });
    };
    // the profile the pickers stand for: a model, another model of a
    // manufacturer, or none ("not listed"); undefined until chosen
    var chosenProfile = function () {
      var mfr = $("reg-mfr").value, model = $("reg-model").value;
      if (mfr === "-") { return ""; }
      return mfr && model ? model : undefined;
    };
    var ready = function () {
      $("reg-go").disabled = !($("reg-name").value.trim() && chosenProfile() !== undefined && reg.getAttribute("data-available") === "true");
    };
    $("reg-mfr").addEventListener("change", function () {
      var mfr = $("reg-mfr").value, any = false;
      $("reg-model").value = "";
      Array.prototype.forEach.call($("reg-model").options, function (o) {
        if (!o.value) { return; }
        o.hidden = o.getAttribute("data-mfr") !== mfr;
        any = any || !o.hidden;
      });
      $("reg-model-box").hidden = !any;
      ready();
    });
    $("reg-model").addEventListener("change", ready);
    $("reg-name").addEventListener("input", ready);
    ready();
    var fail = function (msg) { $("reg-err").textContent = msg; $("reg-err").hidden = false; };
    var stop = function () { if (timer) { window.clearInterval(timer); timer = null; } };
    var poll = function () {
      window.fetch("/api/v1/registration-windows/" + win.id, { credentials: "same-origin" })
        .then(function (r) { return r.ok ? r.json() : Promise.reject(new Error("status " + r.status)); })
        .then(function (w) {
          var left = Math.max(0, Math.round((new Date(w.expires_at) - Date.now()) / 1000));
          $("reg-left").textContent = "The window closes in " + left + " s.";
          if (w.state === "completed") {
            stop(); show("reg-done");
            $("reg-done").innerHTML = "";
            var p = document.createElement("p");
            p.textContent = "The handset is registered as " + $("reg-name").value.trim() + ". ";
            var a = document.createElement("a");
            a.href = w.handset_id ? "/handsets/" + w.handset_id : "/handsets";
            a.textContent = "Give it a phone line";
            p.appendChild(a);
            $("reg-done").appendChild(p);
          } else if (w.state !== "open") {
            stop(); show("reg-start");
            fail(w.state === "expired" ? "No handset registered within the window." : "The registration was cancelled.");
          }
        })
        .catch(function () { /* keep polling; a lost request is not the end */ });
    };
    $("reg-go").addEventListener("click", function () {
      $("reg-err").hidden = true;
      window.fetch("/api/v1/registration-windows", {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ digits: 8, name: $("reg-name").value.trim(), profile: chosenProfile() || "" })
      }).then(function (r) {
        return r.json().then(function (j) { return r.ok ? j : Promise.reject(new Error(j.detail || j.error || ("status " + r.status))); });
      }).then(function (w) {
        win = w; $("reg-pin").textContent = w.pin.replace(/(\d{4})(?=\d)/g, "$1 ");
        show("reg-open"); poll(); timer = window.setInterval(poll, 2000);
      }).catch(function (e) { fail("Could not open the window: " + e.message); });
    });
    $("reg-cancel").addEventListener("click", function () {
      stop();
      if (win) { window.fetch("/api/v1/registration-windows/" + win.id, { method: "DELETE", credentials: "same-origin" }); }
      win = null; show("reg-start");
    });
  }
})();

// a field with data-select-all selects its text when clicked (an inline onclick would not run: the content policy forbids it)
document.addEventListener("click", function (e) {
  var t = e.target;
  if (t && t.matches && t.matches("[data-select-all]") && t.select) { t.select(); }
});
