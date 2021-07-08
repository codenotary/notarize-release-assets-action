# GitHub action for notarizing release assets using CodeNotary's VCN

This official action from CodeNotary allows to notarize all the assets of a release.
It can be triggered from a GitHub workflow when a `release` event happens.

## Usage

See [./.github/workflows/test.yml](.github/workflows/test.yml) for a full example.

- :information_source: If the `cnil_api_key` input is specified, that API key :key: will be used to notarize every asset of every release.
- :information_source: Otherwise the action code will create the necessarry API key(s) :key: (or rotate them if they exist) using GitHub user(name)s (followed by a fixed `@github` suffix) as signer IDs (i.e. for the API key name and prefix):
   - For the uploaded release assets, the  GitHub user(name) :bust_in_silhouette: that uploaded the asse(s) will be used.
   - For the source code archives :package: (zip and tar.gz) an API key :key: will be created/rotated for the GitHub user(name) :bust_in_silhouette: that authored the release (since these archives are are not uploaded, but created automatically by GitHub, hence they have no uploader information).
   - Usually the release author and the assets uploader are one and the same GitHub user :bust_in_silhouette:, hence usually a single API key :key: will be created/rotated for a release.
   - API key example: `ghuser1@github.aoZjJgZSaojYqqLINUhfkIkvXxikbNoValxI`

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
