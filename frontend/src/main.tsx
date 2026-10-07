import React from 'react'
import {createRoot} from 'react-dom/client'
import './style.css'
import App from './App'
import {ErrorBoundary} from './components/ErrorBoundary'

const container = document.getElementById('root')

const root = createRoot(container!)

// The outer boundary is the last line of defence: without it any render error
// unmounts the whole tree and the user is left with an empty window.
root.render(
    <React.StrictMode>
        <ErrorBoundary area="app">
            <App/>
        </ErrorBoundary>
    </React.StrictMode>
)
