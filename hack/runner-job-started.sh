#!/bin/sh
# runner-job-started.sh - the self-hosted runners' job-started hook
# (docs/ci-runners.md "No pull request on these runners"): each
# instance's .env sets ACTIONS_RUNNER_HOOK_JOB_STARTED to a root-owned
# copy of this file, /opt/runner-hooks/job-started.sh, and the runner
# runs it before a job's first step - a non-zero exit fails the job
# there.
#
# A pull request runs the workflow files of its own head: anyone's fork
# could point ci.yml's runs-on at these machines, where the runner's
# user has sudo, the Docker socket, KVM, the lab's network and a key
# that publishes on apt.sw-servers.net. GitHub's approval of outsiders'
# runs is one click away from that. The workflows meant for these
# runners start only from a push to this repository or a dispatch by
# someone with write access, so the runner takes those two events and
# refuses every other job. A new trigger on a self-hosted workflow
# needs a line here first, and a new copy on every machine.
set -eu

repository=swenske/Janus
allowed="push workflow_dispatch"

refuse() {
	echo "Refused by this runner's job-started hook ($0): $1."
	echo "Jobs from pull requests never run on Janus's self-hosted runners - see docs/ci-runners.md."
	exit 1
}

[ "${GITHUB_REPOSITORY:-}" = "$repository" ] ||
	refuse "repository '${GITHUB_REPOSITORY:-}' isn't $repository"

for event in $allowed; do
	[ "${GITHUB_EVENT_NAME:-}" = "$event" ] && exit 0
done
refuse "event '${GITHUB_EVENT_NAME:-}' isn't one of: $allowed"
