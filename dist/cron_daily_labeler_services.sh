#!/bin/bash -e

cd /home/leo/projects/bluesky/get_labelers

echo "processing firehose data..."
./process_firehose_logs.sh

echo "aggregating..."
./aggregate.sh

echo "starting services..."
export DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"$(id -u "$LOGNAME")"/bus
./start_services.sh
