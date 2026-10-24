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
- **host_metrics {host, metric, from?, to?}**: gives one host's CPU, memory, disk, load or network
  usage, building the PromQL itself from node-exporter's own conventions, so nothing (a person or
  the assistant's rules engine) needs to write PromQL just to ask "CPU on www-1". `metric` is one
  of `cpu`, `memory` (or `mem`, `ram`), `disk`, `load`, `network` (or `traffic`). The host is
  matched against the **instance label** (exactly, or with a `:<port>` suffix) or, if that finds
  nothing, node-exporter's **nodename** label (`node_uname_info`), whichever has data; both label
  names are configurable (below). Returns the same kind of result as `query_metrics` (each series'
  last, min, max and avg, and up to 120 downsampled points), titled, for instance, "CPU on www-1
  (last hour)".
- **list_hosts {}**: lists the distinct values of the instance and nodename labels, through
  Grafana's datasource resource call (the same route Explore's own label picker uses). Returns a
  plain JSON array, such as `[{"host":"www-1"},{"host":"localhost:9090"}]`: meant to be read by the
  assistant's dictionary (see below), not by a person.

All seven tools only read: none of them changes anything in Grafana. Each checks that the person
behind the call may use the tools at all (their community is allowed by the setting below) and
that they themselves can already see the channel or community they're asking from; it never
answers with more than that.

### Host name escaping (host_metrics, list_hosts)

`host_metrics`' `host` argument (and the app's own job filter setting) end up inside a PromQL
label matcher's string literal, such as `instance=~"<host>(:[0-9]+)?"`. A host containing `"` or a
regex metacharacter (`.`, `(`, `{`, `|`, ...) must not be able to break out of that matcher, either
to run arbitrary PromQL or to widen the regex match past the one host asked for. `server/hostmetrics.go`'s
`escapeLabelValueForPromQL` handles both: it regex-escapes the value (`regexp.QuoteMeta`, which also
escapes a literal backslash) and then escapes the result for the PromQL string literal itself
(doubling every backslash, escaping every `"`), the same two-layer escaping Go's own `regexp.QuoteMeta`
output needs when written into a Go string literal. `server/hostmetrics_test.go` checks this against
real injection attempts (a value ending the string early, or trying to add an `or vector(1)` clause);
it was also checked by hand against a live Prometheus (see the task report): the unescaped form breaks
the query with a parse error, proving the injection, and the escaped form runs safely with no match.

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
- **Instance label**: the Prometheus label `host_metrics` and `list_hosts` match a host name
  against. Defaults to `instance` (node-exporter's own convention). Only change this if your
  Prometheus relabels scrape targets under a different label.
- **Job filter** (optional): restricts `host_metrics` and `list_hosts` to one Prometheus job (such
  as `node`), so a Prometheus that scrapes more than node-exporter doesn't mix other jobs' series
  into a host's metrics or the host dictionary. Blank (the default) matches every job.

## Teaching the assistant host names (DictionarySources)

The assistant's rules engine has a built-in "resource usage" pattern ("cpu|memory|disk|load on
{host}") that picks a tool named `*host_metrics*` and fills its `host`/`metric` arguments from
whatever it recognises in the message. For it to recognise a host name at all, that name has to be
in the assistant's dictionary, built from each community's **Assistant** settings
`DictionarySources`: a JSON list of `{tool, type, field}` entries, each naming a read-only tool
whose result is scanned for every string under a key named `field`, added to the dictionary as an
entity of `type`.

`list_hosts`' plain `[{"host": "..."}, ...]` result is built for exactly this: set `DictionarySources`
to (this also keeps this app's own built-in dashboard and datasource sources, since setting
`DictionarySources` at all replaces the built-in list rather than adding to it):

```json
[
  {"tool": "*search_dashboards", "type": "dashboard", "field": "title"},
  {"tool": "*list_datasources", "type": "service", "field": "name"},
  {"tool": "*list_hosts", "type": "host", "field": "host"}
]
```

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
- `server/hostmetrics.go` builds `host_metrics`' PromQL from node-exporter conventions (with the
  host escaped against PromQL injection and regex widening), resolves a host to its instance label
  via nodename when the instance label alone finds nothing, and lists `list_hosts`' host names
  through Grafana's datasource resource call.
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
