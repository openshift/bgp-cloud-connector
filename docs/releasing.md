# Releasing

## New product minor

A new product minor (for example `1.0` to `1.1`) is the Konflux `product_version`. It is not the image tag. Image tags stay `{{ labels.version }}`. A z-stream does not change `product_version`, so it skips this section.

`prodsec/bgp-cloud-connector.yaml` in [konflux-release-data](https://gitlab.cee.redhat.com/releng/konflux-release-data) builds the identifiers from that version and `rhel_target: el9`:

- stream `bgp-cc-<version>`
- CPE `cpe:/a:redhat:bgp_cloud_connector:<version>::el9`

### 1. [konflux-release-data](https://gitlab.cee.redhat.com/releng/konflux-release-data)

Open an MR that edits the existing operator ReleasePlanAdmissions only:

- `config/kflux-prd-rh02.0fk9.p1/product/ReleasePlanAdmission/bgp-cloud-connector/bgp-cloud-connector-stage.yaml`
- `config/kflux-prd-rh02.0fk9.p1/product/ReleasePlanAdmission/bgp-cloud-connector/bgp-cloud-connector-prod.yaml`

Set `spec.data.releaseNotes.product_version` to the new minor and the topic to `The <version> release of BGP cloud connector.`

Leave the FBC RPAs on `product_version: fbc`. Leave the ReleasePlans; they already select these RPAs. Leave `prodsec/bgp-cloud-connector.yaml`.

Open this MR before the stream exists. `test_stream_names_are_in_prodsec_repo` job fails on the prod RPA until ProdSec registers it. That failure is the signal they use. The stage RPA is skipped by that test. Example: [!24210](https://gitlab.cee.redhat.com/releng/konflux-release-data/-/merge_requests/24210).

### 2. CPE assignment

Post in `#wg-cpe-assignments` Slack channel with the new stream, the CPE, and the konflux-release-data MR. ProdSec adds the stream to [product-definitions](https://gitlab.cee.redhat.com/prodsec/product-definitions/).

Rerun the konflux-release-data pipeline after their MR merges. The prod stream test should pass.

### 3. [pyxis-repo-configs](https://gitlab.cee.redhat.com/releng/pyxis-repo-configs)

Add the minor to `bgpcc-releases` in `products/bgp-cloud-connector/bgp-cloud-connector.yaml`. The operator and bundle repositories both use that list. Leave the product listing alone. Cicada applies the file after merge; do not change the cicada repo.

Example: [!1952](https://gitlab.cee.redhat.com/releng/pyxis-repo-configs/-/merge_requests/1952) added `1.1`.

### 4. This repository

Set `LABEL cpe` to the same CPE in `Containerfile.bgp-cloud-connector` and `Containerfile.bgp-cloud-connector-bundle`. The advisory CPE comes from the RPA; the image label has to match it.

## Shipping the version

These steps apply to a new minor and to a z-stream.

1. Write the version in `VERSION` and run `make set-version`. That copies it into the `version` and `release` labels of both Containerfiles. Then run `make bundle` so the checked-in OLM bundle picks up the same version from `VERSION`. Example: [#193](https://github.com/openshift/bgp-cloud-connector/pull/193).
2. After that pull request merges, merge the Konflux nudging pull request that updates the operator image digest in the bundle. Example: [#190](https://github.com/openshift/bgp-cloud-connector/pull/190).
3. After the nudging pull request has merged, check that the stage release built from it is green. Continue only then.
4. Create the production `Release` for the operator and bundle (`releasePlan: bgp-cloud-connector-release-plan-prod`). Set `spec.snapshot` to the snapshot of the green stage release.
5. Add that released production bundle to the `stable` channel in every catalog template, currently `catalog/v4.22/catalog-template.yaml` and `catalog/v5.0/catalog-template.yaml`, and regenerate each catalog (`make generate-catalog OCP_VERSION=4.22` and `make generate-catalog OCP_VERSION=5.0`). The bundle image must be `registry.redhat.io/bgpcc/bgp-cloud-connector-operator-bundle@sha256:…`. `registry.stage.redhat.io` fails the FBC enterprise contract (`olm.allowed_registries_related`). Example: [#157](https://github.com/openshift/bgp-cloud-connector/pull/157).
6. After that pull request merges, a stage release is triggered for each FBC catalog. Check that each of those stage releases is green. Continue only then.
7. Create one production `Release` per FBC catalog (`bgp-cloud-connector-fbc-v4-22-release-plan-prod` and `bgp-cloud-connector-fbc-v5-0-release-plan-prod`). Set each `spec.snapshot` to the snapshot of that catalog's green stage release.
