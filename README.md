# Grafana

An Antimatter app that gives the assistant and agents read-only tools to look up Grafana
dashboards, metrics and alerts. There is no chat UI of its own: everything it does is through its
AI tools, and everything it needs is configured in the System Console.

## Features

- **search_dashboards {query}**: searches Grafana dashboards by title. Returns each match's title,
  uid, folder and URL.
- **get_dashboard {uid}**: gives one dashboard's panels, with each panel's title and the queries it
  runs.
- **query_metrics {expr, datasource_uid?, from?, to?, step?}**: runs a PromQL query through
  Grafana's own datasource query API (`/api/ds/query`), the same one Grafana's dashboards and
  Explore use. Returns each series' label summary, its last, min, max and avg values, and up to 120
  downsampled points, plus an Explore link.
- **panel_data {dashboard_uid, panel_id, from?, to?}**: runs one dashboard panel's own queries (not
  a new one) and returns the same kind of result as query_metrics, plus a dashboard link. Also adds
  a rendered panel image link when the `grafana-image-renderer` plugin is installed on Grafana
  (detected and cached for a few minutes); otherwise it says the image isn't available.
- **list_alerts {state?}**: lists Grafana-managed alert rules and their current state (inactive,
  pending or firing), via Grafana's alerting API.

All five tools only read: none of them changes anything in Grafana. Each checks that the person
behind the call may use the tools at all (their community is allowed by the setting below) and
that they themselves can already see the channel or community they're asking from; it never
answers with more than that.

**Not yet (v0.1.0):** link previews for Grafana URLs pasted into a message. Planned as a
follow-up.

## Settings

Configured entirely in **System Console > Apps > Grafana**; there is no settings page in the app
itself.

- **Grafana URL**: the URL this server uses to reach Grafana. Required. Unlike this app's sibling
  integrations for public services, private network addresses are allowed here, since Grafana is
  usually on the server's own internal network; link-local addresses (`169.254.0.0/16`,
  `fe80::/10`) and known cloud metadata addresses are still refused, whatever hostname resolves to
  them.
- **Public Grafana URL** (optional): used instead of the URL above for the dashboard and Explore
  links in tool results, when members' browsers reach Grafana at a different address than this
  server does.
- **Service account token**: a Grafana service account token. Viewer rights are enough: this app
  only reads dashboards, metrics and alerts. Stored encrypted by the server; never sent to the web
  app and never written to a log.
- **Let every community use the Grafana tools**: on by default. Turn it off and list specific
  communities (by name or ID, one per line) to limit the tools to them.

## How it works

- `server/client.go` talks to the Grafana HTTP API with the service account token, through a
  dialer (`server/netguard.go`) that refuses link-local and cloud metadata addresses, a 10 second
  timeout per request and a 5 MiB response cap.
- `server/ai_tools.go` declares the tools (`RegisterAITools`, in `OnActivate`) and runs them
  (`ExecuteAITool`): it checks the app is configured, that the call is allowed
  (`server/community.go`) and within the per-person rate limit (`server/ratelimit.go`), then
  dispatches to a handler per tool.
- `server/format.go` turns a `/api/ds/query` result into the label summary, stats and downsampled
  points every tool shares.
- `server/renderer.go` caches whether `grafana-image-renderer` is installed, so `panel_data` isn't
  checking on every call.

## Development

```sh
make dist    # build the app bundle in dist/
make test    # run the server tests
make check-style
```

Set `MM_SERVICESETTINGS_ENABLEDEVELOPER=true` to only build the server for the current platform.

## Releases

Pushing a `v*` tag builds the app and publishes it as a
[GitHub release](https://github.com/Antimatterchat-Community/antimatter-plugin-grafana/releases)
([release workflow](.github/workflows/release.yml), using the organisation's shared
[app release workflow](https://github.com/AntimatterChat/.github/blob/main/.github/workflows/app-release.yml)).
The version is the tag without its `v`. Tags with a `-`, such as `v1.2.0-rc.1`, are published as
prereleases. Each release has:

- `com.antimatterchat.grafana-<version>.tar.gz`: the app bundle, with the server for Linux (amd64,
  arm64), macOS (amd64, arm64) and Windows (amd64)
- `com.antimatterchat.grafana-<version>.tar.gz.sig`: its detached signature with the Antimatter
  plugin signing key
- `SHA256SUMS`: the checksums of the release files

Install the bundle with **Upload Plugin** in **System Console > Plugins > Plugin Management** or
`amctl plugin install-url <bundle URL>` (both need `AM_PLUGINSETTINGS_ENABLEUPLOADS=true`), see
[Installing other apps](https://antimatter.chat/docs/admins/apps.html).

## Feedback and support

Found a bug in the Grafana app, or missing a feature? Please [open an
issue](https://github.com/AntimatterChat/community/issues/new/choose) in the [Antimatter community
repository](https://github.com/AntimatterChat/community), where the feedback for every part of
Antimatter is gathered. Questions and ideas are welcome in its
[Discussions](https://github.com/AntimatterChat/community/discussions). Please report security
problems privately, as explained in the [security
policy](https://github.com/AntimatterChat/.github/blob/main/SECURITY.md).

## License

GNU Affero General Public License v3.0, see [LICENSE.txt](LICENSE.txt). The build tooling is
derived from Apache-2.0 licensed Mattermost plugins, see [NOTICE.txt](NOTICE.txt).
