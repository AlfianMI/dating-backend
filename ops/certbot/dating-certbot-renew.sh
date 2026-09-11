#!/bin/bash
set -euo pipefail

CERTBOT_DIR="/opt/dating-backend/certbot/conf"
WEBROOT_DIR="/opt/dating-backend/certbot/www"

docker run --rm \
  -v "${CERTBOT_DIR}:/etc/letsencrypt" \
  -v "${WEBROOT_DIR}:/var/www/certbot" \
  certbot/certbot:latest \
  renew \
  --quiet

docker exec dating_nginx_prod nginx -t
docker exec dating_nginx_prod nginx -s reload
