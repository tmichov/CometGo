import './App.css'
import RoutesList from './routes'
import { getDDPClient } from './ddp'

getDDPClient()

function App() {
	return (
		<>
			<RoutesList />
		</>
	)
}

export default App
