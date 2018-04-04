# Post-Release TODO's

This document explains steps that need to be taken after a new release
of the ingress-gce controller has been cut and pushed.

## Update Manifest

The glbc.manifest in the main kubernetes repository needs to be updated
to use the new image. The file is located at kubernetes/cluster/gce/manifests/glbc.manifest.
Make sure to not only update the image for the container but also update the
name in the top-level metadata field as well as version field under metadata.labels.

## Update e2e Tests  

Our e2e tests need to be updated in order to make use of the new release.

### ci-ingress-gce-upgrade-e2e

Find a file called config.json under test-infra/jobs/config.json. In this file,
find the json block called `ci-ingress-gce-upgrade-e2e`. In this block, modify
the environment variable `GCE_GLBC_IMAGE` to point to the latest release image.

### ci-ingress-gce-downgrade-e2e

Find a file called nodes_util.go under kubernetes/test/e2e/framework.
In this file, find a function called ingressUpgradeGCE(). In this function,
find the comment `Downgrade to latest release image`. Below this comment,
you will find the variable `command` being set. Update the image reference
in the set logic for that variable to the latest release image.
