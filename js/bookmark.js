!function() {
  function paypalGetAllFee(){
    vv = [];
    document.querySelectorAll('[aria-labeledby="txnFee"]').forEach(i => vv.push(i.textContent));
    alert(vv.map(i => i.replace(/\D/g, '') / 100).reduce((a, b) => a + b, 0))
  }
  paypalGetAllFee()

}()