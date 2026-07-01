# Standalone Telemetry Module

This telemetry module is for use with cmgr multi host support. It runs a simple go server that returns a json with the format {"overloaded": True/False} depending on the status of the server.
To hit the endpoint do http://<ip_OR_URL>:2316/health
By default it runs on port 2316 so make sure cmgr doesn't assign this port for a challenge! This setting is configurable
It returns overloaded when cpu or ram >90%, it doesn't go back to fine until both drop below 80%

### Ansible Support

Eventually, we want ansible to pull an artifact from this build and then automatically install this server on the challenge containers, but that is for the future.

### HTTPS Support

If we want HTTPS support then we will have this guy serve using the docker certs as well
