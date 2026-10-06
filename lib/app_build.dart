const allSourcesEnabled = bool.fromEnvironment('ALL_SOURCES');
const appName = allSourcesEnabled ? '真果鉴' : '红果鉴';
const appSlug = allSourcesEnabled ? 'zhenguojian' : 'hongguojian';
const duanjuBuildNumber = int.fromEnvironment('BUILD_NUMBER', defaultValue: 80);
const appPackageId = 'com.duanju.duanju_app';
