module github.com/Azure/CetiRun/app/cetirun-agent-broker

go 1.27.3

require github.com/Azure/CetiRun/app/internal v0.1.1

replace github.com/Azure/CetiRun/app/internal => ../internal

replace github.com/Azure/CetiRun/app/cetirun-operator => ../cetirun-operator
