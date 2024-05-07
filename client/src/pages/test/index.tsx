import { getDDPClient } from "../../ddp"

function Index() {
	const client = getDDPClient()
	const handleClick = () => {
		client.subscribe('users')	
	}
	return (
		<>
			<h1>Index</h1>
			<p>
				Edit <code>test</code> and save to test HMR
			</p>
			<button onClick={handleClick}>Click me</button>
		</>
	)
}

export default Index

