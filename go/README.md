# TypeAgent Go Code

## Overview

**[TypeAgent](../README.md)** is **sample code** that explores an architecture for building a _personal agent_ with _natural language interfaces_ leveraging current advances in LLM technology.

This directory contains Go code that supports our **TypeAgent** implementation.

## Components

- [TaskPilot](./taskpilot/) - a lightweight workflow runner for schema-validated task graphs. It helps automate repeatable workflows that mix deterministic steps (scripts, shell commands, etc.) with controlled LLM reasoning.  TaskPilot runs independent tasks in parallel and caches outputs by content-addressed inputs.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of Microsoft
trademarks or logos is subject to and must follow
[Microsoft's Trademark \& Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general).
Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or imply Microsoft sponsorship.
Any use of third-party trademarks or logos are subject to those third-party's policies.
