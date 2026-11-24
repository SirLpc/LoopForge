# Private GitLab over HTTP

An administrator can allow HTTP GitLab addresses on a trusted private network. Set `AGENTFLOW_GIT_EGRESS_ALLOW_CIDRS=192.168.50.10/32` in the platform service environment, or in `.env` for Docker Compose. Use the GitLab IP or the narrowest CIDR it needs. If the API and workers run separately, give them the same setting. The platform and the execution environment must both be able to reach GitLab.

In personal settings, add a **Git access token** with platform **GitLab** and host `http://192.168.50.10:8443`. Use the same HTTP address as the origin of the project repository, such as `http://192.168.50.10:8443/group/repo.git`. Verify the token before running the workflow. Keep this on a trusted network because HTTP carries Git and API credentials in plaintext.

Deploy through the existing source build to load the setting. Use `./start.sh dev` for Compose, or `redeploy-wsl.bat` for a WSL installation with a native Runner. The standard build includes HTTP GitLab support.
