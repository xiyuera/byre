"""Make `​```mermaid` fences usable by mermaid.js.

pymdownx's `fence_code_format` emits:

    <pre class="mermaid"><code>flowchart LR
      a["agent&lt;br/&gt;identity"] --> b["byre"]</code></pre>

mermaid 11 reads the element's **innerHTML**, not its textContent. So it receives
`<code>flowchart LR ...</code>` and answers "No diagram type detected matching given configuration".
That is the whole reason the diagrams did not render: measured by handing a `<pre><code>` element and a
plain element to `mermaid.run()` in the browser -- the first failed with the source visible in the error
message, the second rendered.

The fix could be either "drop the `<code>` wrapper" or "stop escaping the source". Dropping the wrapper
is enough, but then `&lt;br/&gt;` would reach the parser as literal text and the line breaks inside node
labels would show up as visible markup. So this hook does both, narrowly:

  * removes the `<code>` wrapper, and
  * turns `&lt;br/&gt;` back into a real `<br/>` tag, because that is the one HTML construct the diagrams
    actually use and it has to be a tag for mermaid's htmlLabels to break the line.

Everything else stays escaped. A blanket `html.unescape()` would let a stray `<` or `&` in a diagram
break the surrounding page, and mermaid source is written by hand in the repository.
"""

import re

_FENCE = re.compile(
    r'<pre class="mermaid"><code>(?P<src>.*?)</code></pre>',
    re.DOTALL,
)

_BR = (
    ("&lt;br/&gt;", "<br/>"),
    ("&lt;br&gt;", "<br>"),
    ("&lt;br /&gt;", "<br />"),
)


def on_page_content(html, page, config, files):
    """Rewrite mermaid fences into what the renderer expects."""

    def replace(match):
        src = match.group("src")
        for escaped, tag in _BR:
            src = src.replace(escaped, tag)
        return '<pre class="byre-mermaid">%s</pre>' % src

    return _FENCE.sub(replace, html)
