require('dotenv').config();
const { notarize } = require('@electron/notarize');

exports.default = async function notarizing(context) {
  const { electronPlatformName, appOutDir } = context;  
  if (electronPlatformName !== 'darwin') {
    return;
  }

  if (!process.env.KOPIA_UI_NOTARIZE) {
    console.log('Not notarizing because KOPIA_UI_NOTARIZE is not set');
    return;
  }

  const appName = context.packager.appInfo.productFilename;

  console.log('Submitting app for Apple notarization...')
  let timerId = setInterval(() => { console.log('Still waiting for notarization response...') }, 30000);
  let x =  await notarize({
    appBundleId: 'io.kopia.ui',
    appPath: `${appOutDir}/${appName}.app`,
    appleId: process.env.APPLEID,
    appleIdPassword: process.env.APPLEIDPASS,
  });
  clearTimeout(timerId);
  return x;
};