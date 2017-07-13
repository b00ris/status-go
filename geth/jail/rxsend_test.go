package jail_test

var rxSendJS = `
var status = {
    message_id: '42' // global message id, gets replaced in sendTransaction (or any other method)
};

var _status_catalog = {
    commands: {},
    responses: {}
};

var context = {};

function addContext(ns, key, value) { // this function is expected to be present, as status-go uses it to set context
    if (!(ns in context)) {
        context[ns] = {}
    }
    context[ns][key] = value;
}

function call(pathStr, paramsStr) {
    var params = JSON.parse(paramsStr),
        path = JSON.parse(pathStr),
        fn, res;

    // Since we allow next request to proceed *immediately* after jail obtains message id
    // we should be careful overwritting global context variable.
    // We probably should limit/scope to context[message_id] = {}
    context = {};

    fn = path.reduce(function(catalog, name) {
        if (catalog && catalog[name]) {
            return catalog[name];
        }
    }, _status_catalog);

    if (!fn) {
        return null;
    }

    // while fn wll be executed context will be populated
    // by addContext calls from status-go
    callResult = fn(params);
    res = {
        result: callResult,
        // So, context could contain {eth_transactionSend: true}
        // additionally, context gets "message_id" as well.
        // You can scope returned context by returning context[message_id],
        // however since serialization guard will be released immediately after message id
        // is obtained, you need to be careful if you use global message id (it
        // works below, in test, it will not work as expected in highly concurrent environment)
        context: context[status.message_id]
    };

    return JSON.stringify(res);
}

var status = {
    message_id: '42',
};

function send(params) {
    console.log("Recieving send: ", params);
    var data = {
        from: params.from,
        to: params.to,
        value: web3.toWei(params.value, "ether")
    };

    var hash = web3.eth.sendTransaction(data);

    return { "transaction-hash": hash };
};

function sendAsync(params) {
    console.log("Recieving sendAsync: ", params);

    var data = {
        from: params.from,
        to: params.to,
        value: web3.toWei(params.value, "ether")
    };

    var hash

    web3.eth.sendTransaction(data, function(hash) {
        hash = hash
    });

    return { "transaction-hash": hash };
};

var _status_catalog = {
    commands: {
        send: send,
        sendAsync: sendAsync,
    },
    responses: {},
};
`
