
variable "project_id" {
  type        = string
  description = "The project ID to deploy CREMA to."
}

variable "region" {
  type        = string
  description = "The region to deploy the CREMA Cloud Run service to."
  default     = "us-central1"
}
