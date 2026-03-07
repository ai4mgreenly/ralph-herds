#!/bin/sh
set -e

reset-repo

go build -o ralph-herds .

clear

exec ./ralph-herds
