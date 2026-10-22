output "service_url" {
  description = "Cloud Run URL of the edgecache service."
  value       = google_cloud_run_v2_service.edgecache.uri
}

output "image_repository" {
  description = "Docker repository path to push to (GitHub variable GCP_AR_REPO)."
  value       = "${google_artifact_registry_repository.edgecache.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.edgecache.repository_id}"
}

output "workload_identity_provider" {
  description = "Full provider resource name for google-github-actions/auth (GitHub variable GCP_WIF_PROVIDER)."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "deployer_service_account" {
  description = "Service account the workflow impersonates (GitHub variable GCP_DEPLOY_SA)."
  value       = google_service_account.deployer.email
}

output "runtime_service_account" {
  description = "Service account the Cloud Run revisions run as."
  value       = google_service_account.runtime.email
}
