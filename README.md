# home-assistant-grafana-relay

 
 
<p align="center">
 <img height="170" src="https://user-images.githubusercontent.com/1719781/138470470-d96ed6b8-0a07-44ef-8af3-7feb7e0f01f2.png"></br>
   <a href="https://grafana.com">Grafana</a> ❤️ <a href="https://www.home-assistant.io">Home-assistant</a></br></br>
</p>

Listens for alerts sent via webhooks from [Grafana](https://grafana.com) and relays them
as notifications using [Home Assistant](https://www.home-assistant.io/). Alerts appear in
the Home Assistant app and can also be used in smart-home automations.

The relay includes a notification history web UI. Open the relay URL in a browser, for
example `http://relay-host:12000/`, to see complete alert messages, delivery status,
Grafana details, search, and state filters. Grafana can continue posting webhooks to the
same URL: `GET /` displays the UI and `POST /` receives an alert. Existing custom webhook
paths also continue to accept `POST` requests.

## Configuration

Configuration is done via environment variables. 

| Variable | Description | Default / example |
|---|---|---|
| `AUTH_TOKEN` | Home Assistant long-lived access token | `LONG_LIVED_ACCESS_TOKEN` |
| `HM_SERVICE_URI` | Home Assistant notification service endpoint | `http://home.domain.tld/api/services/notify/notify` |
| `LISTEN_PORT` | Webhook and web UI port | `12000` |
| `LISTEN_HOST` | Address to listen on | `0.0.0.0` |
| `NOTIFICATION_CHANNEL` | Optional [Android notification channel](https://companion.home-assistant.io/docs/notifications/notifications-basic/#notification-channels) | `my_channel_name` |
| `HISTORY_FILE` | Optional JSON file used to retain history across restarts | Empty (memory only); Docker uses `/data/notifications.json` |
| `HISTORY_LIMIT` | Maximum number of notifications retained | `200` |
| `WEB_UI_USERNAME` | Optional HTTP Basic username for the UI and history API | Empty |
| `WEB_UI_PASSWORD` | HTTP Basic password; must be set with `WEB_UI_USERNAME` | Empty |
| `WEBHOOK_SECRET` | Optional secret accepted in `X-Webhook-Secret` or a Bearer authorization header | Empty |

History is newest-first and includes failed Home Assistant deliveries. When `HISTORY_FILE`
is empty, history remains available until the relay restarts. The configured path must be
writable by the relay user. Writes use a private, atomically replaced file.

### Notification history UI

Visit `http://relay-host:12000/`. The page refreshes every 30 seconds and does not truncate
notification text. Alert image and rule URLs are links; images are not fetched automatically.
The underlying read-only JSON endpoint is `GET /api/notifications`, and `GET /healthz` is
available for health checks.

The UI is unauthenticated unless both `WEB_UI_USERNAME` and `WEB_UI_PASSWORD` are set. Do
not expose the relay directly to the internet. Bind it to a trusted interface, use the UI
credentials, or place it behind an authenticated HTTPS reverse proxy. `WEBHOOK_SECRET` can
also protect incoming webhooks if your Grafana configuration or reverse proxy can send the
matching header.

### Show it inside Home Assistant

Home Assistant's current [Webpage card](https://www.home-assistant.io/dashboards/iframe/)
can embed the history page in a dashboard. Add a Webpage card in the dashboard editor, or
use the following card YAML:

```yaml
type: iframe
url: https://relay.example.com/
aspect_ratio: 100%
title: Grafana alerts
```

You can make that dashboard visible in the Home Assistant sidebar. The relay URL must be
reachable by the browser viewing Home Assistant. Browsers will not embed an HTTP relay page
inside an HTTPS Home Assistant page, so use HTTPS on the relay/reverse proxy when Home
Assistant uses HTTPS.

### Home-assistant

Generate a [long-lived access
token](https://developers.home-assistant.io/docs/auth_api/#long-lived-access-token)
in Home Assistant. Tokens can be created in the profile section (`https://home.domain.tld/profile`).

### Grafana

Create a new notification channel in Grafana under
`https://mydomain.tld/alerting/notification/new` of type `webhook`. Make sure to
check the `Include Image` checkbox. As URL enter where this relay service will
be listening, e.g. `http://relay-host:12000/`.

### Example

In production you might want to write systemd service. A minimal working setup
looks like this.

```
export AUTH_TOKEN="LONG_LIVED_ACCESS_TOKEN"
export HM_SERVICE_URI="http://home.domain.tld/api/services/notify/notify"
export LISTEN_PORT="12000"
export LISTEN_HOST="localhost"
export HISTORY_FILE="./notifications.json"
./home-assistant-grafana-relay
```

### Run with docker-compose

To build and run the service with docker, you can use the `Dockerfile`.
In production you can use docker-compose to start the service automatically:

1. clone this repository
2. create `.env` from `.env.example`
3. `cd home-assistant-grafana-relay`
4. `docker-compose up -d`

The Compose configuration mounts a named volume at `/data`, so the default history file
survives container replacement. To remove the stored history as well as the containers,
you must explicitly remove that volume.

## NixOS module

A flake.nix file is included for compatibility with [Nix
Flakes](https://nixos.wiki/wiki/Flakes) for those that wish to use it as a
module. A bare-minimum flake.nix would be as follows:

```nix
{
  description = "NixOS configuration";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    ha-relay.url = "github:pinpox/home-assistant-grafana-relay";
  };

  outputs = inputs@ { ha-relay, nixpkgs, ... }: {
    nixosConfigurations = {
      hostname = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        specialArgs = { inherit inputs; };
        modules = [
          ./configuration.nix
          ha-relay.nixosModules.ha-relay
          {
            pinpox.services.home-assistant-grafana-relay = {
              enable = true;
              listenHost = "localhost";
              listenPort = "12000";
              haUri = "https://home.domain.com/api/services/notify/notify";
              envFile = "/var/secrets/ha-envfile";
            };
          }
        ];
      };
    };
  };
}
```

The envfile can be used to provide the token without putting it in the nix
store.

```env
AUTH_TOKEN="LONG_LIVED_ACCESS_TOKEN"
```
