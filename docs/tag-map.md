# Tag map

The repository history was rewritten on 2026-11-17 to remove internal addresses
from every commit, including commit messages and author identities. A rewrite
changes every commit hash, so any record written before it — a release note, a
`build-info.txt`, a task comment — that names a commit hash now names an object
that no longer exists.

**The stable identifier is the tag name, not the hash.** This file records what
each tag points at after the rewrite, and what the one externally published
release referred to before it.

## What each tag points at now

| tag | tag object | commit |
|---|---|---|
| `v0.1.0` | `17545b797ccaabac064b393ad4e57764bfaf6039` | `7a1c9be0f26527ffb10ea43420bd7af140efd436` |
| `v0.1.1` | `ff02d88e22667c0543107ba2c6ad532c9fd39d5a` | `20b08e608653bbf9220b2085a4cfdf3e04da3cef` |
| `v0.1.2` | `dfe18e186f3233a06fe6da648f588cd0cb25a994` | `d1ec434b960e2247cc1e8d45844839db659635b4` |
| `v0.2.0` | `b1e5eccdcf5b86579855bd0aecadbdcf0082332d` | `0ee8c66a055ae16a88dcb7e8c01f53e415b7e4fb` |
| `v0.3.0` | `04a0712b0b661b226320d420845c5e848544e37e` | `c1727d39a3d72c0ef2582008a41eaa8e3e081ea3` |
| `v0.3.1` | `8167e8e35848d3692d982d955b744602d28aad15` | `76489ee738eac8aed3af3f90257d2fd5b98fcbbd` |
| `main` | — | `d4b1ff4a7337bfb8d0d2672117889446197cf1dd` |

## The one hash that was published and no longer exists

`v0.3.0`'s release assets were built before the rewrite, so its
`build-info.txt` records:

```
commit=1aecc0a0f206f656494251ee11d6502f2c1de90a
reproduce=git clone <repository> && cd byre && git checkout 1aecc0a0f2...
```

`1aecc0a0f206...` is not in the repository any more (`git cat-file -e` returns
128). The equivalent content is `v0.3.0^{commit}` = `c1727d39a3d72c0ef2582008a41eaa8e3e081ea3`.
The published binaries are functionally identical — the rewrite only replaced
address strings — but that `reproduce=` line cannot be followed verbatim.

`v0.3.1` onwards is built from the rewritten history and its `build-info.txt`
names a commit that exists, so this file is only needed for the transition.

## Verifying

```sh
git rev-parse v0.3.0^{commit}          # the commit this file claims
git cat-file -e 1aecc0a0^{commit}      # 128 = correctly gone
git merge-base --is-ancestor v0.3.0^{commit} main && echo "tag is on main"
```
