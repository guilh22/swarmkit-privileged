# [SwarmKit-Privileged](https://github.com/guilh22/swarmkit-privileged)

[![PkgGoDev](https://img.shields.io/badge/go.dev-docs-007d9c?logo=go&logoColor=white&style=flat-square)](https://pkg.go.dev/github.com/moby/swarmkit)
[![CI Status](https://img.shields.io/github/actions/workflow/status/guilh22/swarmkit-privileged/ci.yml?branch=master&label=ci&logo=github&style=flat-square)](https://github.com/guilh22/swarmkit-privileged/actions?query=workflow%3Aci)
[![Go Report Card](https://goreportcard.com/badge/github.com/guilh22/swarmkit-privileged)](https://goreportcard.com/report/github.com/guilh22/swarmkit-privileged)
[![codecov](https://img.shields.io/codecov/c/github/guilh22/swarmkit-privileged?logo=codecov&style=flat-square)](https://codecov.io/gh/guilh22/swarmkit-privileged)

## Overview

*SwarmKit-Privileged* is a fork of the original [SwarmKit](https://github.com/moby/swarmkit) project. This fork's primary goal is to enable the use of the `privileged` flag for containers managed by Docker Swarm, allowing certain applications requiring elevated permissions to run in Swarm.

**Important:** The `privileged` flag should only be used by users who fully understand the security implications. Running containers in privileged mode can expose the host system to serious security vulnerabilities.

For all other documentation and usage, you can still refer to the original [SwarmKit repository](https://github.com/moby/swarmkit). This fork mainly adds support for the `privileged` flag, and all other features remain consistent with the upstream project.

**Special ShoutOut**: A big thanks to [olljanat](https://github.com/olljanat/swarmkit) for most of the groundwork that made this fork possible.

---

## Key Changes in This Fork

- **Support for `privileged` Containers**: This fork introduces support for the `privileged` flag in Docker Compose and Swarm services. This allows containers to run with full privileges, similar to how Kubernetes handles privileged containers.

  Example `docker-compose.yml`:

  ```yaml
  version: '3.8'
  services:
    myservice:
      image: nginx
      privileged: true
      deploy:
        replicas: 1
  ```

- **Security Considerations**: Enabling privileged containers grants elevated access to the host system, which can compromise its security. Only use the `privileged` flag in environments where the risks are well-understood and mitigated. We recommend using this feature only when absolutely necessary and in controlled environments.

---

## Features

Apart from the `privileged` container support, *SwarmKit-Privileged* retains all the original SwarmKit features, including:

- **Distributed**: Uses the [Raft Consensus Algorithm](https://raft.github.io/) for coordination and fault tolerance.
- **Secure**: Provides mutual TLS for secure node communication and certificate rotation.
- **Simple**: Operates without requiring external databases or complex setups.
- **Orchestration**: Provides desired state reconciliation, service types, configurable updates, and restart policies.
- **Scheduling**: Aware of node resources and constraints to place tasks efficiently.
- **Cluster Management**: Dynamic role changes, node draining, and more for flexible cluster management.
- **Security**: Offers mutual TLS, token-based joins, and automated certificate rotations.

---

## Security Warning

**Warning:** The `privileged` flag grants containers root-level access to the host machine, which can significantly increase the attack surface and compromise system security. It should only be used by experienced administrators who understand the potential risks and can manage them properly.

**DO NOT** use the `privileged` flag unless you have a clear understanding of what it does and why it’s necessary for your use case.

---

For more detailed usage examples and features, please refer to the [original SwarmKit documentation](https://github.com/moby/swarmkit).
