# GitHub REST API

Service: `github`. Origin: `https://api.github.com`. Base path: `/`.

- GET `/repos/{owner}/{repo}` for repository metadata.
- GET `/installation/repositories` to enumerate repositories available to a
  GitHub App installation token. The binding can further restrict the token to
  selected repositories.
- GET `/repos/{owner}/{repo}/issues`, query `{"state":"open","per_page":"20"}`.
  GitHub's issues endpoint also returns pull requests; entries with
  `pull_request` are PRs. Filter those when the user asks specifically for issues.
- GET `/repos/{owner}/{repo}/pulls`, query `{"state":"open","per_page":"20"}`.
- GET `/repos/{owner}/{repo}/contents/{path}` for JSON content metadata; file
  text may be base64 in `body.content`. Raw/binary media is unsupported.
- POST `/repos/{owner}/{repo}/issues`, body `{"title":"Example","body":"Details"}`.
- POST `/repos/{owner}/{repo}/pulls`, body includes `title`, `head` and `base`.

These routes use repository owner/name; a numeric repository ID is not required.
Use the returned `Link` header for pagination after validating and converting
its URL as described in the main skill. The optional `X-GitHub-Api-Version`
header selects a supported API version.

Check the connection mode and binding returned by `connections_status`.
GitHub App installation connections use installation tokens scoped to the
binding's selected repositories and permissions. GitHub records the App's bot
as the actor. Personal OAuth connections act as the connected user and remain
subject to that user's repository rights and organization OAuth/SSO restrictions.

For an installation connection, do not use `/user` or `/user/repos` as a health
check: those user-token endpoints can return `403 Resource not accessible by
integration` even when repository access works. Use the exact repository route
and the binding's `issues`, `contents`, or `pull_requests` permissions. The
repository response's user-role `permissions` object alone does not establish
whether an installation token can perform an issue or pull-request operation.

`unauthenticated` and `authorization_changed` from the connection service are
platform authorization errors, not GitHub permission responses. They do not by
themselves establish that the owner must reconnect GitHub. Report the failing
tool and stage; request reconnection when the service reports that owner
interaction is required. A failed response after creating an issue or PR can
leave the operation's outcome unknown; check for its result before resubmitting.
