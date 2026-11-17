#!/usr/bin/env bash
# Translation drift check for the *.zh-CN.md pairs.
#
# docs/.i18n.json records, for each translation, the digest of the English file it was made from. A
# translation whose recorded digest no longer matches the source is older than the text it claims to
# translate, which is the only failure this script calls a failure.
#
#   0  nothing stale
#   1  at least one stale translation
#   2  the check could not run (missing manifest, no digests) — never reported as success
set -u
cd "$(dirname "$0")/.." || exit 2
manifest="docs/.i18n.json"

digest() { sha256sum "$1" | cut -d' ' -f1; }
pairs() { find . -name '*.zh-CN.md' -not -path './.git/*' | sort; }

if [ "${1:-}" = "--update" ]; then
  {
    printf '{\n'; first=1
    for zh in $(pairs); do
      en="${zh%.zh-CN.md}.md"
      [ -f "$en" ] || { echo "no English source for $zh" >&2; exit 2; }
      [ $first -eq 1 ] || printf ',\n'; first=0
      printf '  "%s": {"source": "%s", "digest": "%s"}' "$zh" "$en" "$(digest "$en")"
    done
    printf '\n}\n'
  } > "$manifest"
  echo "manifest updated: $manifest ($(grep -c digest "$manifest") entries)"
  exit 0
fi

[ -r "$manifest" ] || { echo "missing $manifest (run: scripts/check-i18n.sh --update)" >&2; exit 2; }
command -v python3 >/dev/null && command -v sha256sum >/dev/null || exit 2

python3 - "$manifest" <<'PY'
import hashlib,json,sys,pathlib
man=json.load(open(sys.argv[1]))
if not man: sys.exit("empty manifest: run scripts/check-i18n.sh --update")
d=lambda p: hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest()
stale=[]
for zh,rec in sorted(man.items()):
    src=rec["source"]
    if not pathlib.Path(src).exists(): stale.append((zh,src,"source missing")); continue
    if d(src)!=rec["digest"]: stale.append((zh,src,"translation older than source"))
print(f"checked={len(man)} stale={len(stale)}")
for zh,src,why in stale: print(f"  STALE {zh} <- {src}: {why}")
tracked={'README.md','architecture.md','operations.md','release.md','i18n.md'}
for p in sorted(pathlib.Path('.').rglob('*.md')):
    if '.git' in p.parts or p.name.endswith('.zh-CN.md'): continue
    if p.name not in tracked: continue
    if not pathlib.Path(str(p).replace('.md','.zh-CN.md')).exists(): print(f"  UNTRANSLATED {p}")
sys.exit(1 if stale else 0)
PY
