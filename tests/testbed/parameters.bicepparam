using './template.bicep'

param location = 'centralus'
param vmCount = 3
param vmNamePrefix = 'jaco'

param virtualNetworkName = 'vnet-centralus-1'
param subnetName = 'snet-centralus-1'
param vnetAddressPrefix = '172.16.0.0/16'
param subnetAddressPrefix = '172.16.0.0/24'

param virtualMachineSize = 'Standard_B2s'
param osDiskType = 'StandardSSD_LRS'
param osDiskSizeGiB = 128

param adminUsername = 'azureuser'

// SSH public key. Sourced from the environment (deploy.sh exports it from
// testbed/.env.local) so no key is committed. A bare `bicep build` lints with
// an empty value; deploy.sh enforces a non-empty key before deploying.
param adminPublicKey = readEnvironmentVariable('ADMIN_PUBLIC_KEY', '')

// Open SSH to the internet by default (neutral bed). Set SSH_SOURCE_CIDR in
// .env.local to your operator IP/CIDR to lock it down.
param sshSourceAddressPrefix = readEnvironmentVariable('SSH_SOURCE_CIDR', 'Internet')

param autoShutdownStatus = 'Enabled'
param autoShutdownTime = '0200'
param autoShutdownTimeZone = 'Eastern Standard Time'
param autoShutdownNotificationEmail = readEnvironmentVariable('SHUTDOWN_NOTIFICATION_EMAIL', '')

// Rendered + base64'd by deploy.sh from cloud-init.yaml.tpl, exported as
// CUSTOM_DATA. Kept out of CLI args (ps-aux-visible) and out of any committed
// file. Empty default lets `bicep build` lint with no env.
param customData = readEnvironmentVariable('CUSTOM_DATA', '')
