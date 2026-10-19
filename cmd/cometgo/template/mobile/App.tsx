import { StatusBar } from "expo-status-bar";
import { useEffect, useState } from "react";
import { StyleSheet, Text, View } from "react-native";
import { CometProvider, useComet } from "@cometgo/client/react";
import { comet } from "./comet";

export default function App() {
  return (
    <CometProvider client={comet}>
      <Home />
    </CometProvider>
  );
}

// Replace with your screens; useSubscription and useComet().call reach the server.
function Home() {
  const c = useComet();
  const [status, setStatus] = useState(c.status);
  useEffect(() => c.onChange(() => setStatus(c.status)), [c]);
  return (
    <View style={styles.container}>
      <Text>CometGo: {status}</Text>
      <StatusBar style="auto" />
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, alignItems: "center", justifyContent: "center" },
});
