import { StatusBar } from "expo-status-bar";
import { useEffect, useState } from "react";
import { StyleSheet, Text, View } from "react-native";
import { comet } from "./comet";

export default function App() {
  const [status, setStatus] = useState(comet.status);
  useEffect(() => comet.onChange(() => setStatus(comet.status)), []);
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
