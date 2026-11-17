# Contributing from an agent

Every commit so far is authored by `keypod contributors <noreply@example.com>`. That identity is honest
about *what* produced a change and useless about *who*: the log cannot tell a review from an
implementation, and a mistake cannot be routed back to the role that made it.

## One account per role

| Role | Account | Commit author |
|---|---|---|
| implementation | `<prefix>-eng` | `<prefix>_eng <noreply+eng@<domain>>` |
| operations, runbooks | `<prefix>-ops` | `<prefix>_ops <noreply+ops@<domain>>` |
| licence, ToS, contracts | `<prefix>-legal` | `<prefix>_legal <noreply+legal@<domain>>` |
| external research | `<prefix>-research` | `<prefix>_research <noreply+research@<domain>>` |
| review | `<prefix>-reviewer` | `<prefix>_reviewer <noreply+reviewer@<domain>>` |

The account name is the accountability boundary; two roles sharing one account puts the log back where it
started. Concrete values for a deployment live in the credential file below (CONTRIBUTING rule 3); except
for the reviewer row, confirm a row with `git log -1 --format='%an <%ae>'` on that role's first commit.

The author comes from `$GIT_AUTHOR_NAME` / `$GIT_AUTHOR_EMAIL`; never override it with a `-c user.name=…`
flag, which is how commits acquire `x <x@x>` — worse than the placeholder, because it looks attributable.

## Credentials and git authentication

A skill that declares `required_credential_files` makes the backend write one file per service to
`~/.hermes/credentials/`, mode `0600`. For Gitea it exports `GITEA_HOST`, `GITEA_ACCESS_TOKEN`,
`GITEA_USER`, `GIT_AUTHOR_*` / `GIT_COMMITTER_*` and `CURL_CA_BUNDLE` / `GIT_SSL_CAINFO`:

```sh
. ~/.hermes/credentials/gitea
[ -f "$CURL_CA_BUNDLE" ] || unset CURL_CA_BUNDLE GIT_SSL_CAINFO   # a real trust store, never -k
```

A missing bundle fails loudly (`error setting certificate file`), which is right; answering it with `-k`
downgrades every later call in the same script, so fall back to the system trust store, and if that does not
verify either, fix the trust store. Never source this file in a script run with `sh -x` and never echo the
token: a trace prints the expansion into the transcript; print `${#GITEA_ACCESS_TOKEN}` instead.

```sh
export GIT_ASKPASS="$HOME/.hermes/credentials/git-askpass.sh" GIT_TERMINAL_PROMPT=0
git remote set-url origin "https://${GITEA_USER}@<gitea-host>/<owner>/<repo>.git"
```

The helper answers git's prompts from the environment, so the token never reaches the URL, `argv`,
`.git/config` or the process list; `GIT_TERMINAL_PROMPT=0` fails instead of hanging unattended.

## Issues and pull requests

Build the JSON with `jq` and let `curl` read it from stdin (`-d @-`), so the body stays in a file that can be
reviewed before it is posted; keep `html_url` and the API `number` from the response, because an issue that
cannot be cited is not a report:

```sh
jq -n --rawfile body issue.md --arg title "$title" '{title:$title, body:$body}' \
  | curl -sS -X POST "$GITEA_HOST/api/v1/repos/<owner>/<repo>/issues" \
      -H "Authorization: token $GITEA_ACCESS_TOKEN" -H 'Content-Type: application/json' -d @-

# a pull request is the same call on /pulls, with head and base added to the object:
#   '{title:$title, body:$body, head:$head, base:"main"}'
```

## Branches and review

Branch per task, named for the role: `review/<slug>`, `ops/<slug>`, `eng/<slug>`. Never commit to `main`
and never force-push it. Messages follow CONTRIBUTING (`<area>: <imperative summary>`), then
`git push -u origin HEAD`.

`<prefix>-reviewer` opens issues and pull requests and reviews them; it does not merge and does not push
to `main`. A review agent that can merge is a review agent whose approval means nothing. Reviewer comments
state the command that was run and its result; an opinion without a command is not evidence.

## Tokens

Scopes are per role and minimal: `write:repository` only for roles that push. CLI-issued tokens carry no
expiry in this deployment, so treat one as a per-task credential: issue it, use it, then revoke it with
`DELETE $GITEA_HOST/api/v1/users/<account>/tokens/<token-name>`. Revoking needs a broader scope than
`write:repository`, so the revoker is a short-lived token revoked last. A token that has ever appeared in
output, a log or a transcript gets revoked and reissued: the exposure is the event, not the moment it is
noticed.
