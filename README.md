# resume-as-code

My resume, built and deployed the way I actually build things: Terraform,
AKS, GitOps, and a live observability stack, with a Go app that's both the
site and a running demo of the platform under it.

See `ARCHITECTURE.md` for the full system design, `BOOTSTRAP.md` for the
things that have to be set up by hand outside this repo, `infra/README.md`
for the Terraform usage, and the site itself for the human-readable resume.

## Status

Work in progress - infra/ (AKS cluster + networking) is in place. app/,
manifests/, and the CI/CD workflows are next.
