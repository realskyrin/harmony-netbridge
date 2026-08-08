import appWhitelistTest from './AppWhitelist.test';
import dnsMessageTest from './DnsMessage.test';
import hnbProtocolTest from './HnbProtocol.test';
import installedApplicationsTest from './InstalledApplications.test';
import reconnectPolicyTest from './ReconnectPolicy.test';
import proxyCertificateTest from './ProxyCertificate.test';
import proxyDiagnosticsTest from './ProxyDiagnostics.test';

export default function testsuite(): void {
  appWhitelistTest();
  dnsMessageTest();
  hnbProtocolTest();
  installedApplicationsTest();
  reconnectPolicyTest();
  proxyCertificateTest();
  proxyDiagnosticsTest();
}
