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

  // the position picker of a base station's page
  var mapEl = document.getElementById("station-map");
  var lat = document.getElementById("lat"), lon = document.getElementById("lon");
  if (mapEl && window.L && lat && lon) {
    var tiles = mapEl.getAttribute("data-tiles");
    var has = lat.value !== "" && lon.value !== "";
    var start = has ? [parseFloat(lat.value), parseFloat(lon.value)] : [51, 10];
    var map = window.L.map(mapEl, { attributionControl: !!tiles }).setView(start, has ? 14 : 4);
    if (tiles) {
      window.L.tileLayer(tiles, { maxZoom: 19, attribution: mapEl.getAttribute("data-attribution") || "" }).addTo(map);
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
      window.L.tileLayer(ptiles, { maxZoom: 19, attribution: pub.getAttribute("data-attribution") || "" }).addTo(pm);
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
    };
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
            p.textContent = "The handset is registered. ";
            var a = document.createElement("a");
            a.href = w.handset_id ? "/handsets/" + w.handset_id : "/handsets";
            a.textContent = "Name it and give it a line";
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
        headers: { "Content-Type": "application/json" }, body: JSON.stringify({ digits: 8 })
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
