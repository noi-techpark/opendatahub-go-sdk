<!--
SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>

SPDX-License-Identifier: CC0-1.0
-->

# OpenDataHub Official Go SDK

## Package `ingest`

### Golang libs for Open Data Hub ingestion microservices

These libraries are for use inside the Open Data Hub to implement Data collectors, Transformers and Elaborations

#### dc
Data collector specific utilities. Services that push to a rabbitmq exchange

#### tr
Transformer specific utilities. Services that listen on a rabbitmq queue

#### urn
Helper functions to parse and build URN for raw data naming

#### rdb
Raw Data Bridge client to get raw data without contacting the underlying storage(s)

#### ms (microservices)
General utility boilerplate like logging, configuration and error handling

## Package `qmill`

[Watermill](https://github.com/ThreeDotsLabs/watermill) wrapper to semplify connection and interaction with RabbitMQ (AMPQ protocol).

## Package `tel`

Pluggable Preconfigured Telemetry provider (tracing, metrics and logging).

## Package `testsuite`

Helpers for testing OpenDataHub related logics.