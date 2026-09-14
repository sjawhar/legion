# Deployment instructions (demo)

Replace this file in your overlay with the deployment's standing rules — required checks,
deploy and smoke commands, code-owner expectations, the roles to consult, the merge
credential. Every process this daemon launches (root architects, sub-architects, phase workers,
the controller) carries it as the last part of its system prompt; the daemon refuses to start
when the file is missing or blank.
