#!/bin/bash

redis-server --slaveof ${REDISMASTER_SERVICE_HOST:-$SERVICE_HOST} $REDISMASTER_SERVICE_PORT
