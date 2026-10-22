// Commands shown after creating an app or a VPN key. Bundles go into an env file or a prompt,
// never onto a command line that ends up in shell history.

const REPO = "k18k/entrypoint";

export const installScript = (version: string) =>
  `curl -fsSLO https://github.com/${REPO}/releases/download/v${version}/install-connector.sh`;

export function tunnelSnippets(version: string, service: string, health?: string) {
  const h = health ? ` --health ${health}` : "";
  return {
    systemd: `${installScript(version)}
sudo sh install-connector.sh tunnel --service ${service} --upstream http://127.0.0.1:3000${h}
# paste ENTRYPOINT_CONNECT when asked; the service starts at boot`,
    node: `npm install @k18k/entrypoint-sdk

// server.js
import { expose } from "@k18k/entrypoint-sdk/http";
await expose(app, { service: "${service}"${health ? `, healthCheck: "${health}"` : ""} });
// run with ENTRYPOINT_CONNECT in the environment`,
    docker: `services:
  app:
    image: your-app
  tunnel:
    image: ghcr.io/${REPO.split("/")[0]}/entrypoint-connector:${version}
    command: ["tunnel", "--upstream", "http://app:3000"${health ? `, "--health", "${health}"` : ""}]
    environment:
      ENTRYPOINT_CONNECT: \${ENTRYPOINT_CONNECT}   # keep it in .env, outside the repository
    stop_grace_period: 40s
    restart: unless-stopped`,
  };
}

export function exposeSnippets(version: string, name: string, port: string, local: string) {
  const expose = local && local !== `127.0.0.1:${port}` ? `${port}=${local}` : port;
  return {
    systemd: `${installScript(version)}
sudo sh install-connector.sh vpn --name ${name} --expose ${expose}
# paste ENTRYPOINT_VPN when asked`,
    docker: `services:
  ${name}-vpn:
    image: ghcr.io/${REPO.split("/")[0]}/entrypoint-connector:${version}
    command: ["vpn", "--name", "${name}", "--expose", "${port}=<service>:${port}"]
    environment:
      ENTRYPOINT_VPN: \${ENTRYPOINT_VPN}
    volumes: ["${name}-vpn:/state"]
    restart: unless-stopped
volumes:
  ${name}-vpn: {}`,
  };
}

export function forwardSnippets(version: string, me: string, target: string, port: string) {
  return {
    systemd: `${installScript(version)}
sudo sh install-connector.sh vpn --name ${me} --forward ${target}:${port}
# paste ENTRYPOINT_VPN when asked; ${target} then resolves to 127.0.0.1 on this machine
# e.g. postgresql://user:pass@${target}:${port}/db`,
    docker: `services:
  app:
    image: your-app
    environment:
      DATABASE_URL: postgresql://user:pass@${target}:${port}/db
  ${target}:   # the service name is the host name the app connects to
    image: ghcr.io/${REPO.split("/")[0]}/entrypoint-connector:${version}
    command: ["vpn", "--name", "${me}", "--forward", "0.0.0.0:${port}=${target}:${port}"]
    environment:
      ENTRYPOINT_VPN: \${ENTRYPOINT_VPN}
    volumes: ["${me}-vpn:/state"]
    restart: unless-stopped
volumes:
  ${me}-vpn: {}`,
  };
}

export const tailscaleJoin = (server: string, key: string) => `sudo tailscale up --login-server ${server} --auth-key ${key}`;
