// The parts of the React Native modules the react-native entry imports, so
// the package builds without them installed. They are optional peer
// dependencies; the apps that use the entry have the real ones.
declare module "react-native-get-random-values" {}
declare module "react-native" {
  export const AppState: import("./native-lifecycle.ts").AppStateLike;
}
declare module "@react-native-community/netinfo" {
  const NetInfo: import("./native-lifecycle.ts").NetInfoLike;
  export default NetInfo;
}
