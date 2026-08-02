// Package workflows holds no production code: it exists so tests can pin
// load-bearing details of the GitHub workflow files — the release tag
// pattern and the job dependencies — which nothing else would catch before
// a tag push.
package workflows
