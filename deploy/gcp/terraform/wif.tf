# Workload Identity Federation for GitHub Actions: the deploy workflow trades
# its GitHub OIDC token for a short-lived Google token. No service account key
# is ever created, stored, or rotated.

resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "github"
  display_name              = "GitHub Actions"
  description               = "OIDC federation for GitHub Actions deploys"

  depends_on = [google_project_service.apis]
}

resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github-oidc"
  display_name                       = "GitHub OIDC"

  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
    "attribute.ref"        = "assertion.ref"
    "attribute.workflow"   = "assertion.workflow"
  }

  # Reject every token not minted for this repository and ref before any IAM
  # binding is even consulted (forks and other repos cannot get a token).
  attribute_condition = "assertion.repository == \"${var.github_repository}\" && assertion.ref == \"${var.github_deploy_ref}\""

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

# The identity the workflow impersonates. All of its grants are on single
# resources (one repo, one service, one service account), none project-wide.
resource "google_service_account" "deployer" {
  account_id   = "${var.service_name}-deploy"
  display_name = "edgecache GitHub Actions deployer"
}

resource "google_service_account_iam_member" "deployer_wif" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository/${var.github_repository}"
}

# Push images to this one repository.
resource "google_artifact_registry_repository_iam_member" "deployer_push" {
  location   = google_artifact_registry_repository.edgecache.location
  repository = google_artifact_registry_repository.edgecache.name
  role       = "roles/artifactregistry.writer"
  member     = google_service_account.deployer.member
}

# Roll new revisions of this one service (run.services.get/update).
resource "google_cloud_run_v2_service_iam_member" "deployer_deploy" {
  name     = google_cloud_run_v2_service.edgecache.name
  location = google_cloud_run_v2_service.edgecache.location
  role     = "roles/run.developer"
  member   = google_service_account.deployer.member
}

# A revision runs as the runtime account, so the deployer must be allowed to
# act as it (iam.serviceAccounts.actAs) - on that account only.
resource "google_service_account_iam_member" "deployer_acts_as_runtime" {
  service_account_id = google_service_account.runtime.name
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.deployer.member
}
