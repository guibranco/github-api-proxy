#!/usr/bin/env bash
# One-time host preparation for github-api-proxy on a vinhedo node. Run as root, from a repository
# checkout or an extracted release tarball:
#
#   sudo deploy/scripts/bootstrap.sh [deploy-user] [proxy-domain] [admin-domain]
#   sudo ./bootstrap.sh deploy github-proxy.example.com github-proxy-api.example.com
#
# It installs the parts that must be trusted and therefore must NOT come from a deploy upload:
#   /usr/local/sbin/github-api-proxy-install   root-owned copy of install.sh (what sudoers allows)
#   /etc/systemd/system/github-api-proxy.service
#   /etc/sudoers.d/github-api-proxy            lets the deploy user run only that installer
# plus the service user and directories and, when both domains are given and NGINX is installed,
# the NGINX site (HTTP; add TLS with certbot afterwards). Re-run it whenever install.sh, the unit or
# the NGINX template change in the repository (install.sh warns when a release ships a different
# copy). The deploy user's SSH key is left for you to add.
set -euo pipefail

DEPLOY_USER="${1:-deploy}"
PROXY_DOMAIN="${2:-}"
ADMIN_DOMAIN="${3:-}"
APP=github-api-proxy
SVC_USER=github-proxy
APP_ROOT=/opt/${APP}
CONF_DIR=/etc/${APP}
INSTALLER_DST=/usr/local/sbin/${APP}-install
UNIT_DST=/etc/systemd/system/${APP}.service
SUDOERS_DST=/etc/sudoers.d/${APP}
NGINX_DST=/etc/nginx/sites-available/${APP}.conf

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Repository layout first, then the flat layout of a release tarball.
INSTALLER_SRC="${HERE}/install.sh"
UNIT_SRC="${HERE}/../systemd/${APP}.service"
[[ -f "${UNIT_SRC}" ]] || UNIT_SRC="${HERE}/${APP}.service"
NGINX_SRC="${HERE}/../nginx/${APP}.conf"
[[ -f "${NGINX_SRC}" ]] || NGINX_SRC="${HERE}/${APP}.nginx.conf"

if [[ $EUID -ne 0 ]]; then
  echo "bootstrap.sh must run as root (use sudo)" >&2
  exit 1
fi
[[ -f "${INSTALLER_SRC}" ]] || { echo "missing ${INSTALLER_SRC}" >&2; exit 1; }
[[ -f "${UNIT_SRC}" ]] || { echo "missing systemd unit next to bootstrap.sh" >&2; exit 1; }
[[ "${DEPLOY_USER}" =~ ^[a-z_][a-z0-9_-]*$ ]] || { echo "invalid deploy user name '${DEPLOY_USER}'" >&2; exit 1; }
domain_re='^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$'
for d in "${PROXY_DOMAIN}" "${ADMIN_DOMAIN}"; do
  [[ -z "${d}" || "${d}" =~ ${domain_re} ]] || { echo "invalid domain '${d}'" >&2; exit 1; }
done

echo "▶ Service user and directories"
if ! id -u "${SVC_USER}" >/dev/null 2>&1; then
  useradd --system --home "${APP_ROOT}" --shell /usr/sbin/nologin "${SVC_USER}"
fi
install -d -m 755 "${APP_ROOT}" "${APP_ROOT}/releases"
install -d -m 750 -o root -g "${SVC_USER}" "${CONF_DIR}"

echo "▶ Deploy user '${DEPLOY_USER}'"
if ! id -u "${DEPLOY_USER}" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash "${DEPLOY_USER}"
fi
DEPLOY_HOME="$(getent passwd "${DEPLOY_USER}" | cut -d: -f6)"
install -d -m 700 -o "${DEPLOY_USER}" -g "${DEPLOY_USER}" "${DEPLOY_HOME}/.ssh"
touch "${DEPLOY_HOME}/.ssh/authorized_keys"
chown "${DEPLOY_USER}:${DEPLOY_USER}" "${DEPLOY_HOME}/.ssh/authorized_keys"
chmod 600 "${DEPLOY_HOME}/.ssh/authorized_keys"

echo "▶ Trusted installer → ${INSTALLER_DST}"
install -m 755 -o root -g root "${INSTALLER_SRC}" "${INSTALLER_DST}"
bash -n "${INSTALLER_DST}"

echo "▶ systemd unit → ${UNIT_DST}"
install -m 644 -o root -g root "${UNIT_SRC}" "${UNIT_DST}"
systemctl daemon-reload

echo "▶ sudoers rule → ${SUDOERS_DST}"
SUDOERS_TMP="$(mktemp)"
printf '%s ALL=(root) NOPASSWD: %s\n' "${DEPLOY_USER}" "${INSTALLER_DST}" > "${SUDOERS_TMP}"
visudo -cf "${SUDOERS_TMP}" >/dev/null
install -m 440 -o root -g root "${SUDOERS_TMP}" "${SUDOERS_DST}"
rm -f "${SUDOERS_TMP}"

if [[ -n "${PROXY_DOMAIN}" && -n "${ADMIN_DOMAIN}" ]]; then
  if command -v nginx >/dev/null 2>&1 && [[ -f "${NGINX_SRC}" ]]; then
    echo "▶ NGINX site → ${NGINX_DST}"
    sed -e "s/__PROXY_DOMAIN__/${PROXY_DOMAIN}/g" -e "s/__ADMIN_DOMAIN__/${ADMIN_DOMAIN}/g" "${NGINX_SRC}" > "${NGINX_DST}.tmp"
    mv "${NGINX_DST}.tmp" "${NGINX_DST}"
    ln -sfn "${NGINX_DST}" "/etc/nginx/sites-enabled/${APP}.conf"
    nginx -t
    systemctl reload nginx
  else
    echo "⚠️  NGINX not found (or template missing) — skipping the site; see deploy/nginx/${APP}.conf" >&2
  fi
fi

echo
echo "✅ Host prepared."
echo
echo "This host's SSH key fingerprints — compare them with the output of 'ssh-keyscan -p PORT HOST' run from a"
echo "trusted machine before storing that output as the SSH_KNOWN_HOSTS secret:"
for key in /etc/ssh/ssh_host_*_key.pub; do
  if [[ -f "${key}" ]]; then
    ssh-keygen -lf "${key}" 2>/dev/null | sed 's/^/    /' || true
  fi
done
cat <<NEXT

Next steps:
  1. Put the public half of the SSH_PRIVATE_KEY repository secret in ${DEPLOY_HOME}/.ssh/authorized_keys
  2. Set SSH_HOST=$(hostname -f 2>/dev/null || hostname), SSH_USER=${DEPLOY_USER}, SSH_KNOWN_HOSTS (see above),
     GRIMOIRE_BASE_URL and GRIMOIRE_API_KEY in the repository secrets
  3. Create the settings and secrets in Grimoire (README → Configuration)
NEXT
if [[ -n "${PROXY_DOMAIN}" ]]; then
  echo "  4. TLS: certbot --nginx -d ${PROXY_DOMAIN} -d ${ADMIN_DOMAIN}"
fi
echo "  Then merge to main — the Deploy workflow runs: sudo ${INSTALLER_DST} <release-dir> <env-file>"
