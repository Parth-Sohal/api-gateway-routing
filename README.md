# API Gateway & Reverse Proxy

A simple API Gateway built with Go to understand how requests can be received, processed, and forwarded to backend services using a reverse proxy.

## Architecture

Currently, the project works like this:

```text
                    Client / Postman
                          |
                          | GET /login
                          v
                 +-------------------+
                 |   API Gateway     |
                 |    :8080          |
                 +---------+---------+
                           |
                           | Reverse Proxy
                           v
                 +-------------------+
                 |   Dummy Backend   |
                 |    :9000          |
                 +---------+---------+
                           |
                           | Response
                           v
                 +-------------------+
                 |   API Gateway     |
                 |    :8080          |
                 +---------+---------+
                           |
                           v
                    Client / Postman