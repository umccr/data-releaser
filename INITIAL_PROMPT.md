I am creating a new CLI tool that allows an administrator within the OrcaBus and UMCCR world to specify which `library_id` of files should be shared to a researcher. The input to this tool is a list of `library_id`s and the output is a mechanism that allows the researcher to consume the shared files via a htsget-rs endpoint that has a VPC lattice share to the researcher's AWS account. For now, sharing to
whole AWS accounts is fine.

### Infrastructure

The current set-up involves multiple accounts that hold a dev -> stg -> prod split of microservices within the OrcaBus world, where the prod deployment has access to the production data.
The account that the actual data files are in is separate to the production account, where the prod account has a data share via policies to the data account.

Separately to this, htsget-rs is deployed to a data-control account whose purpose is to share data to consumer accounts (which are the researcher's accounts) via a VPC lattice sharing setup. The VPC lattice
allows a consumer account to hit the htsget-rs endpoint in the data-control account and access only the allowed data. Researchers will be consuming the data through Seqera, which is a workflow engine that allows them to perform analysis. The important point being that they are only allowed to see the data that has been explicitly shared by this new tool using the `library_id`s. 

The relevant AWS accounts for this:
* Production (472057503814) - holds OrcaBus services and infrastructure.
* Data (503977275616) - holds the actual data files that the researchers will access.
* Data control (916218008751) - holds the htsget-rs deployment that gives access to researchers' account via VPC lattice sharing.
* Researchers' accounts - the design should allow for multiple arbitrary accounts that have different `library_id`s shared into them via htsget-rs. For testing, the demo account (534840902377) should be treated as a pilot reseacher's account. 

The base account infrastructure is deployed across two organisations, and uses the following repos:
* https://github.com/umccr/infrastructure - deployment in UMCCR organisation.
* https://github.com/umccr/infrastructure-unimelb - deployment in UniMelb organisation.

### Services

The OrcaBus system is a microservices and event-based orchestration system that runs analysis on bioinformatics data. There is a UI portal that users use to access files, libraries, cases, etc. for curation. In the backend, there are multiple core systems that run workflows to produce results for the UI. The new CLI tool will need to make use of some of these backend services.

The relevant repos in OrcaBus are:
* https://github.com/OrcaBus/service-filemanager - indexes available files.
* https://github.com/OrcaBus/service-fmannotator - annotates filemanager files with a `portal_run_id`.
* https://github.com/OrcaBus/service-metadata-manager - holds library metadata.
* https://github.com/orcaBus/service-workflow-manager - assists in workflow automation.
* https://github.com/OrcaBus/orca-ui - the front-end UI portal.
* https://github.com/OrcaBus/service-shared-resources - shared base resources like a database and event bus.
* https://github.com/OrcaBus/platform-cdk-constructs - shared platform SDK.

The htsget-rs service is a protocol that allows a query to request a certain file, or regions of a file to be downloaded in a two-part ticket response system. It has a rich config set that allows fine-grained access control, and is the basis for the VPC lattice share.

The relevant repos for htsget-rs are:
* https://github.com/umccr/htsget-rs - the service implementation.
* https://github.com/umccr/htsget-deploy - the CDK construct that deploys an AWS Lambda of htsget-rs.
* https://github.com/umccr/htsget-pipeline - the automated htsget-rs deployment for the data control and researchers' accounts specifically.

### Implementation

The new tool should be implemented in Go, and is expected coordinate the sharing of specific library ids into a researchers' account. It should produce some mechanism that allows a research to know which files or library_ids they have access to, and coordinate the actual VPC lattice share. 

There will naturally be a split of what is the infrastructure, and what is a dynamic share of certain library_ids:
* The infrastructure will be the actual htsget-rs deployment in the data control account, and then the VPC share.
* The dynamic share is what htsget-rs will use to authorize a particular request.

It is up to the agent to determine what is the best way to implement this. Some open questions for research include:
* How does the researcher find out about which files they can access, do they recieve a static JSON manifest (in a bucket or emailed), or do they query a small backend API that stores the share?
* How does the CLI tool dynamically tell htsget-rs which files to share, does it use a static config file, a dynamic auth backend server, something else?
* What information is required from the OrcaBus system to achieve this?
* How does the data-control account access data from the data account (this is not yet implemented at the infrastructure level)?

The agent can assume that any repos that are part of the umccr or OrcaBus GitHub organisations can be modified and changed to accomplish this in a way that most makes sense. The tool should start simple
and be able to:
* Initiate a data share.
* Allow it to be time-limited (e.g. stop sharing after 2 weeks)
* Allow it to be disabled/cancelled manually by the tool. 

As part of the process, the agent should question the user and any designs they propose. Nothing is set in stone, and if there is a better way to do something, it should say so.

The agent should also adequately research each of the repositories listed above to gather context, and should ensure that they follow best-practices for designing this software. The agent should also freely use the web to research how best to implement something if it is unsure.

