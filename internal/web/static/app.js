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
})();
