# GitHub action for notarizing release assets using CodeNotary's VCN

This official action from CodeNotary allows to notarize all the assets of a release.
It can be triggered from a GitHub workflow when a `release` event happens.

## Usage

See [./.github/workflows/test.yml](.github/workflows/test.yml) for a full example.

---
### ❗ IMPORTANT tip for `vcn` verification

**The source code archives are slightly different when downloaded from the browser** (i.e. from the GitHub UI for the release) vs from the GitHub API.

The latter has all the source code under a root directory inside the archive, while the first one doesn't have this.

This means that one has to download the source code archives from the GitHub API in order to verify them using vcn. Otherwise `vcn` will (correctly) report the asset as not notarized, since (the structure of) the archive file is slightly different.

- For private repositories: one can download the assets using cURL - e.g.:

   `curl -L "https://api.github.com/repos/<repository-owner>/<repository-name>/zipball/v1.0.0" -H "Authorization: token <GitHub-token>" --output <repository-name>-v1.0.0.zip`
   
   - **NOTE**: the download URL for each asset needs to be copied from the action output.

- For public repositories: one can just copy the assets URLs from the action output and paste them in a browser window to download them.
   - **NOTE**: this won’t work for private repositories because one needs to also specify the GitHub token.
---

## Developer notes: build the Docker image

This action runs as a Docker image.
To produce an artifact for the action from the code, one can build the action
and publish it to DockerHub:

`docker build -t codenotary/notarize-release-assets .`

`docker push codenotary/notarize-release-assets`
