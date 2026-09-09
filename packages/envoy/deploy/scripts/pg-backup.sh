#!/bin/sh

while true; do
  if pg_dump -Fc "$DATABASE_URL" | aws s3 cp - "s3://$DISPATCH_BACKUP_BUCKET/dispatch-$(date -u +%FT%H%M).dump"; then
    sleep 86400
  else
    echo "backup failed; retrying in 1h" >&2
    sleep 3600
  fi
done
