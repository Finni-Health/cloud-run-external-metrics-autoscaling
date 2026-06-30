
output "service_id" {
  value       = google_cloud_run_v2_service.crema.id
  description = "An identifier for the CREMA Cloud Run service with format projects/{{project}}/locations/{{location}}/services/{{name}}."
}

output "service_uri" {
  value       = google_cloud_run_v2_service.crema.uri
  description = "The main URI in which the CREMA Cloud Run service is serving traffic."
}

output "service_name" {
  value       = google_cloud_run_v2_service.crema.name
  description = "The name of the CREMA Cloud Run service."
}
