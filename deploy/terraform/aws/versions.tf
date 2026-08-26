terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # tfstate carries every secret in this stack (DB passwords, console
  # credentials) — a remote, locked, encrypted backend is mandatory for
  # any real deployment. Fill in and uncomment:
  # backend "s3" {
  #   bucket         = "woca-tfstate"
  #   key            = "private-deploy/aws.tfstate"
  #   region         = "us-east-1"
  #   dynamodb_table = "woca-tfstate-lock"
  #   encrypt        = true
  # }
}

provider "aws" {
  region = var.region
}
