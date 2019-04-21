# Levee

## What is Levee?
It is a npm registry proxy and cache. As a proxy it handles communication to internal and external registries in the order they are listed in till one responds with required package info. As a cache, when any registry responds with the package info, it will cache it in a Redis db.
