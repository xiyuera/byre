// Mermaid bootstrap for this site.
//
// Two measured constraints drive this file:
//
// 1. Material 9.8 ships mermaid styles but does not render `.mermaid` blocks by itself -- with only the
//    library loaded the page kept zero <svg>. So something has to call mermaid.
// 2. The call has to wait for content. Running at DOMContentLoaded raced the page: the element existed
//    with empty textContent, mermaid answered "No diagram type detected ... for text: " and the node was
//    already flagged as rendered, so it never recovered. Hence the retry loop: an element is only marked
//    once it actually has source text.
//
// hooks/mermaid.py is what makes the source readable here: it removes the `<code>` wrapper pymdownx emits,
// because mermaid reads innerHTML and would otherwise parse `<code>flowchart LR ...</code>`.
(function () {
  "use strict";

  var MAX_ATTEMPTS = 40; // 40 x 150ms = 6s, generous for a slow first paint
  var seq = 0;

  function scheme() {
    var attr = document.body && document.body.getAttribute("data-md-color-scheme");
    return attr === "default" ? "default" : "dark";
  }

  function injectStyles() {
    if (document.getElementById("byre-mermaid-style")) return;
    var style = document.createElement("style");
    style.id = "byre-mermaid-style";
    // Diagrams are authored dark-first; make sure the SVG does not inherit a white background.
    style.textContent = ".byre-mermaid svg{max-width:100%;height:auto;}" +
      ".byre-mermaid{overflow-x:auto;margin:1.2em 0;}";
    document.head.appendChild(style);
  }

  function attempt(n) {
    if (!window.mermaid) {
      if (n < MAX_ATTEMPTS) return setTimeout(function () { attempt(n + 1); }, 150);
      return;
    }

    var all = Array.prototype.slice.call(document.querySelectorAll(".byre-mermaid"));

    // The script can execute before the page body exists (mkdocs puts `extra_javascript` in the head), in
    // which case there is nothing to find yet and the retry below would never be reached without this.
    if (!all.length) {
      if (n < MAX_ATTEMPTS) return setTimeout(function () { attempt(n + 1); }, 150);
      return;
    }
    var todo = all.filter(function (el) {
      return !el.getAttribute("data-byre-done") && (el.textContent || "").trim().length > 0;
    });

    if (!todo.length) {
      var waiting = all.some(function (el) {
        return !el.getAttribute("data-byre-done") && !(el.textContent || "").trim();
      });
      if (waiting && n < MAX_ATTEMPTS) {
        return setTimeout(function () { attempt(n + 1); }, 150);
      }
      return;
    }

    injectStyles();
    window.mermaid.initialize({
      startOnLoad: false,
      theme: scheme(),
      // `loose` allows the <br/> tags the diagrams use inside node labels.
      securityLevel: "loose",
      fontFamily: "inherit",
      flowchart: { useMaxWidth: true, htmlLabels: true },
      sequence: { useMaxWidth: true },
      gantt: { useMaxWidth: true }
    });

    // Mark before running so a second timer tick cannot pick the same nodes up again.
    todo.forEach(function (el) {
      el.setAttribute("data-byre-done", "1");
      el.setAttribute("data-byre-seq", String(seq++));
    });

    window.mermaid.run({ nodes: todo }).catch(function (e) {
      todo.forEach(function (el) {
        el.setAttribute("data-byre-error", String((e && e.message) || e).slice(0, 200));
      });
    });
  }

  function start() {
    attempt(0);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
