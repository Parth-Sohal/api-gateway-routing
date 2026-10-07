**\# ⚡ API Gateway Routing System**

\> A custom API Gateway built from scratch using Go to understand
routing, reverse proxying, middleware, and communication between
multiple backend services.

**—**

**\## 🧠 About the Project**

This project is a custom **\*\*API Gateway\*\*** built using Go.

The gateway acts as a **\*\*single entry point\*\*** for clients and
forwards requests to different backend services.

Instead of the client directly communicating with every backend service:

\`\`\`text

  ┌──────────────────┐

  │ Login Service │

  │ :9001 │

  └────────▲─────────┘

  │

  │

┌──────────┐ ┌──────┴───────┐

│ Client │ ───────► │ API Gateway │

└──────────┘ :8080 │ :8080 │

  └──────┬───────┘

  │

  ▼

  ┌──────────────────┐

  │ User Service │

  │ :9002 │

  └──────────────────┘

\`\`\`

The main goal is to understand **\*\*how an API Gateway works
internally\*\*** by building its core features step by step.

**—**

**\# 🚀 Current Features**

\- ✅ Go HTTP Gateway

\- ✅ Multiple backend services

\- ✅ Request routing

\- ✅ Reverse proxy

\- ✅ Global logging middleware

\- ✅ Route-specific middleware

\- ✅ Middleware chaining

\- ✅ Postman API testing

\- ✅ Separate backend services

\- ✅ Single entry point for clients

**—**

**\# 🏗️ Current Architecture**

The project currently contains three independent servers.

\`\`\`text

  ┌────────────────────┐

  │ Login Service │

  │ :9001 │

  └─────────▲──────────┘

  │

  │

┌──────────────┐ ┌────────┴─────────┐

│ │ │ │

│ Client │ ───────► │ API Gateway │

│ Postman │ :8080 │ :8080 │

│ │ │ │

└──────────────┘ └────────┬─────────┘

  │

  │

  ┌─────────▼──────────┐

  │ User Service │

  │ :9002 │

  └────────────────────┘

\`\`\`

**\### Ports**

\| Component \| Port \| Purpose \|

\|———–\|——\|———\|

\| API Gateway \| \`8080\` \| Main entry point \|

\| Login Service \| \`9001\` \| Login backend \|

\| User Service \| \`9002\` \| User backend \|

**—**

**\# 📂 Project Structure**

\`\`\`text

api-gateway-routing/

│

├── cmd/

│ │

│ ├── gateway/

│ │ └── main.go

│ │

│ ├── login/

│ │ └── main.go

│ │

│ └── user/

│ └── main.go

│

├── internal/

│ │

│ ├── middleware/

│ │ ├── logging.go

│ │ └── user.go

│ │

│ └── proxy/

│ └── proxy.go

│

├── go.mod

└── README.md

\`\`\`

**—**

**\# 🔀 Request Routing**

The gateway routes requests based on the request path.

**\## \`/login\`**

\`\`\`text

GET /login

  │

  ▼

Gateway :8080

  │

  ▼

Login Proxy

  │

  ▼

Login Service :9001

\`\`\`

**\## \`/users\`**

\`\`\`text

GET /users

  │

  ▼

Gateway :8080

  │

  ▼

User Middleware

  │

  ▼

User Proxy

  │

  ▼

User Service :9002

\`\`\`

The client only communicates with:

\`\`\`text

localhost:8080

\`\`\`

The backend services are accessed through the gateway.

**—**

**\# 🔁 Reverse Proxy**

The gateway uses Go’s standard library:

\`\`\`go

net/http/httputil

\`\`\`

to create reverse proxies.

The main function used is:

\`\`\`go

httputil.NewSingleHostReverseProxy()

\`\`\`

Example:

\`\`\`go

proxy.NewProxy(“http://localhost:9001”)

\`\`\`

Creating the proxy does **\*\*not\*\*** immediately send a request.

It creates a configured proxy handler that knows where requests should
be forwarded.

The actual request is sent when a client request reaches the gateway.

**\### Request Lifecycle**

\`\`\`text

1\. Client sends request

  ↓

2\. Gateway receives request

  ↓

3\. Middleware executes

  ↓

4\. Router selects handler

  ↓

5\. Reverse proxy forwards request

  ↓

6\. Backend processes request

  ↓

7\. Backend sends response

  ↓

8\. Reverse proxy forwards response

  ↓

9\. Client receives response

\`\`\`

**—**

**\# 🧩 Middleware**

Middleware is code that runs before or around an HTTP handler.

Basic structure:

\`\`\`go

func Middleware(next http.Handler) http.Handler {

  return http.HandlerFunc(func(w http.ResponseWriter, r \*http.Request)
{

  // Middleware logic

  next.ServeHTTP(w, r)

  })

}

\`\`\`

The important part is:

\`\`\`go

next.ServeHTTP(w, r)

\`\`\`

This passes the request to the next handler.

Middleware can:

\- Inspect requests

\- Modify requests

\- Modify responses

\- Log information

\- Authenticate users

\- Limit requests

\- Reject requests

\- Add headers

\- Measure request time

**—**

**\# 📝 Logging Middleware**

A global logging middleware has been implemented.

It records:

\- HTTP method

\- Request path

Example:

\`\`\`text

GET /login

GET /users

POST /login

\`\`\`

It uses:

\`\`\`go

r.Method

r.URL.Path

\`\`\`

The flow is:

\`\`\`text

Client

  ↓

Logging Middleware

  ↓

Router

  ↓

Proxy

  ↓

Backend

\`\`\`

Since the logging middleware wraps the router, it can observe requests
coming into the gateway.

**—**

**\# 🎯 Route-Specific Middleware**

Middleware does not have to apply to every route.

The project currently contains a User Middleware that is applied only to
\`/users\`.

\`\`\`go

http.Handle(

  “/users”,

  middleware.UserMiddleware(userProxy),

)

\`\`\`

The flow becomes:

\`\`\`text

/users

  ↓

User Middleware

  ↓

User Proxy

  ↓

User Service

\`\`\`

While \`/login\` goes directly to its proxy:

\`\`\`text

/login

  ↓

Login Proxy

  ↓

Login Service

\`\`\`

The current User Middleware simply prints a message to demonstrate
route-specific middleware.

**—**

**\# 🔗 Middleware Chaining**

Multiple middleware can be combined.

Example:

\`\`\`go

middleware.Logging(

  middleware.Auth(

  userProxy,

  ),

)

\`\`\`

The request flows through them in order:

\`\`\`text

Request

  ↓

Logging

  ↓

Authentication

  ↓

User Proxy

  ↓

Backend

\`\`\`

Middleware order matters because a middleware can stop a request before
it reaches the backend.

For example:

\`\`\`go

if token == “” {

  http.Error(w, “Unauthorized”, http.StatusUnauthorized)

  return

}

\`\`\`

If the middleware returns without calling:

\`\`\`go

next.ServeHTTP(w, r)

\`\`\`

the request stops there.

**—**

**\# 🧪 Testing**

The gateway can be tested using **\*\*Postman\*\***.

**\## Login**

\`\`\`http

GET http://localhost:8080/login

\`\`\`

Request flow:

\`\`\`text

Postman

  ↓

Gateway :8080

  ↓

Login Proxy

  ↓

Login Service :9001

\`\`\`

**\## Users**

\`\`\`http

GET http://localhost:8080/users

\`\`\`

Request flow:

\`\`\`text

Postman

  ↓

Gateway :8080

  ↓

User Middleware

  ↓

User Proxy

  ↓

User Service :9002

\`\`\`

**—**

**\# 🧠 Concepts Learned**

**\## Go**

\- \`net/http\`

\- HTTP servers

\- \`http.Handler\`

\- \`http.HandlerFunc\`

\- \`ServeHTTP\`

\- Go modules

\- Project structure

**\## API Gateway**

\- Gateway architecture

\- Single entry point

\- Backend service separation

\- Request routing

\- Request forwarding

\- Service communication

**\## Reverse Proxy**

\- \`httputil.ReverseProxy\`

\- \`NewSingleHostReverseProxy\`

\- Target URLs

\- Request forwarding

\- Response forwarding

**\## Middleware**

\- Middleware pattern

\- Global middleware

\- Route-specific middleware

\- Middleware chaining

\- Middleware order

\- Request inspection

\- Request modification

\- Request rejection

**—**

**\# 🛠️ Technology Stack**

\| Technology \| Usage \|

\|———–\|——-\|

\| Go \| API Gateway and backend services \|

\| \`net/http\` \| HTTP server and routing \|

\| \`httputil\` \| Reverse proxy \|

\| Postman \| API testing \|

\| Git \| Version control \|

The gateway currently uses Go’s standard library instead of an external
API Gateway framework.

**—**

**\# 🗺️ Development Roadmap**

**\## ✅ Completed**

\- \[x\] Initialize Go project

\- \[x\] Create HTTP gateway

\- \[x\] Create Login Service

\- \[x\] Create User Service

\- \[x\] Run multiple backend services

\- \[x\] Implement reverse proxy

\- \[x\] Implement request routing

\- \[x\] Implement global logging middleware

\- \[x\] Implement route-specific middleware

\- \[x\] Understand middleware chaining

\- \[x\] Test requests using Postman

**\## 🔨 Planned**

\- \[ \] Rate limiting

\- \[ \] Authentication middleware

\- \[ \] Authorization

\- \[ \] Request validation

\- \[ \] Health checks

\- \[ \] Load balancing

\- \[ \] Circuit breaker

\- \[ \] Timeout handling

\- \[ \] Error handling

\- \[ \] Metrics

\- \[ \] Request/response logging

\- \[ \] Header manipulation

\- \[ \] Integration with a real application

**—**

**\# 🎯 Final Goal**

The final version of the project aims to resemble a real API Gateway:

\`\`\`text

  ┌───────────────┐

  │ Auth Service │

  └───────▲───────┘

  │

  │

┌──────────┐ ┌─────┴──────┐

│ │ │ │

│ Client │ ─────────────────►│ API Gateway│

│ │ │ │

└──────────┘ └─────┬──────┘

  │

  ┌───────────┼───────────┐

  │ │ │

  ▼ ▼ ▼

  User Service Order Service Other Services

\`\`\`

With gateway-level functionality such as:

\`\`\`text

Client

  ↓

┌────────────────────────┐

│ API Gateway │

│ │

│ Logging │

│ Rate Limiting │

│ Authentication │

│ Authorization │

│ Routing │

│ Load Balancing │

│ Circuit Breaking │

│ Metrics │

│ Reverse Proxy │

└────────────┬───────────┘

  │

  ▼

  Backend Services

\`\`\`

**—**

**\# 💡 Why Build It From Scratch?**

Instead of treating an API Gateway as a black box, this project focuses
on understanding the actual request lifecycle:

\`\`\`text

Receive

  ↓

Inspect

  ↓

Process

  ↓

Route

  ↓

Forward

  ↓

Receive Response

  ↓

Return Response

\`\`\`

The objective is to understand the engineering concepts behind API
Gateways and distributed backend systems by implementing them step by
step.

**—**

\*\*# 📌 Project Status

**Current Stage:** 🟢 Gateway + Routing + Reverse Proxy + Middleware +
Rate Limiting

**Completed:** Core API Gateway functionality and Token Bucket rate
limiting

**Next Stage:** 🔐 Authentication + Authorization

**Overall Status:** 🚧 Backend system is being developed incrementally
