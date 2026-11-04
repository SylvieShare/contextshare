#!/usr/bin/env bash
# Install only a verified ContextShare bundle on the explicitly selected VM.
set -euo pipefail
stage="$1"
revision="$2"
host="$3"
expected_vm="$4"
[[ "$stage" =~ ^/tmp/contextshare-release\.[A-Za-z0-9]+$ && -d "$stage" && ! -L "$stage" && "$(realpath "$stage")" == "$stage" ]]
[[ "$revision" =~ ^[0-9a-f]{40}$ && "$host" =~ ^[A-Za-z0-9.-]+$ && "$expected_vm" =~ ^[a-z0-9]{20}$ ]]
actual_vm=$(curl -fsS --max-time 4 -H 'Metadata-Flavor: Google' http://169.254.169.254/computeMetadata/v1/instance/id)
[[ "$actual_vm" == "$expected_vm" ]] || { echo 'Unexpected deployment VM' >&2; exit 1; }
cd "$stage"
sha256sum -c SHA256SUMS
[[ "$(od -An -N4 -tx1 contextshare | tr -d ' \n')" == 7f454c46 ]]
sudo -n test -s "/etc/letsencrypt/live/$host/fullchain.pem"
sudo -n test -s "/etc/letsencrypt/live/$host/privkey.pem"
sudo -n test -f /etc/nginx/sites-available/contextshare

exec 9>"$HOME/.contextshare-deploy.lock"
flock -w 20 9
if ! getent passwd contextshare >/dev/null; then
    sudo -n useradd --system --user-group --home-dir /var/lib/contextshare --shell /usr/sbin/nologin contextshare
fi
release="/opt/contextshare/releases/$revision"
sudo -n install -d -m 755 "$release" /etc/contextshare
sudo -n install -m 755 contextshare "$release/contextshare"
sudo -n install -m 644 run.py "$release/run.py"
sudo -n install -m 644 yandex-ca.pem /etc/contextshare/yandex-ca.pem

old=$(readlink /opt/contextshare/current || true)
if [ -n "$old" ] && [[ ! "$old" =~ ^/opt/contextshare/releases/[a-f0-9]{40}$ ]]; then
    echo 'Unexpected current release link' >&2; exit 1
fi
if sudo -n test -f /etc/contextshare/runtime.env; then
    sudo -n cp -p /etc/contextshare/runtime.env "$stage/runtime.previous"
fi
sudo -n cp -p /etc/nginx/sites-available/contextshare "$stage/nginx.previous"
changed=0
rollback(){
    code=$?
    trap - EXIT
    if [ "$code" -ne 0 ] && [ "$changed" -eq 1 ]; then
        echo 'Activation failed; restoring the previous service configuration.' >&2
        sudo -n install -m 644 "$stage/nginx.previous" /etc/nginx/sites-available/contextshare || true
        sudo -n nginx -t && sudo -n systemctl reload nginx || true
        if [ -n "$old" ] && sudo -n test -f "$stage/runtime.previous"; then
            sudo -n install -m 640 -o root -g contextshare "$stage/runtime.previous" /etc/contextshare/runtime.env || true
            sudo -n ln -sfn "$old" /opt/contextshare/next && sudo -n mv -Tf /opt/contextshare/next /opt/contextshare/current || true
            sudo -n systemctl restart contextshare || true
        else
            sudo -n systemctl stop contextshare || true
        fi
    fi
    sudo -n rm -f -- "$stage/runtime.previous" "$stage/nginx.previous"
    exit "$code"
}
trap rollback EXIT
changed=1
sudo -n install -m 640 -o root -g contextshare pilot.json /var/lib/contextshare/pilot.json
sudo -n install -m 640 -o root -g contextshare runtime.env /etc/contextshare/runtime.env
sudo -n install -m 644 contextshare.service /etc/systemd/system/contextshare.service
sudo -n ln -sfn "$release" /opt/contextshare/next
sudo -n mv -Tf /opt/contextshare/next /opt/contextshare/current
sudo -n systemctl daemon-reload
sudo -n systemctl restart contextshare
healthy=0
for _ in $(seq 1 25); do
    health=$(curl -fsS --max-time 2 http://127.0.0.1:8090/healthz 2>/dev/null || true)
    if [[ "$health" == *'"status":"ok"'* && "$health" == *"\"commitSha\":\"$revision\""* ]]; then
        healthy=1
        break
    fi
    sleep 1
done
if [ "$healthy" -ne 1 ]; then
    sudo -n journalctl -u contextshare -n 15 --no-pager
    exit 1
fi
sudo -n install -m 644 nginx.conf /etc/nginx/sites-available/contextshare
sudo -n nginx -t
sudo -n systemctl reload nginx
sudo -n install -m 644 contextshare-certbot.service contextshare-certbot.timer /etc/systemd/system/
sudo -n systemctl daemon-reload
sudo -n systemctl enable contextshare
sudo -n systemctl enable --now contextshare-certbot.timer
echo "ContextShare is ready at https://$host ($revision)"
