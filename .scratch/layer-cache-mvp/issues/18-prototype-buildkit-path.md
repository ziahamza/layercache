# Prototype the Docker BuildKit Local and Team Cache path

Type: prototype
Status: open
Blocked by: 03, 05, 06, 07, 08, 10, 12, 13

## Question

Can Layer Cache configure native BuildKit local and registry caches across Linux and macOS clients, keep target platforms isolated, serialize or merge concurrent publication safely, survive remote failure, and attribute observed cache hits and estimated savings without inventing a Docker cache protocol?
